/*************************************************************************
 * Copyright 2018 Gravwell, Inc. All rights reserved.
 * Contact: <legal@gravwell.io>
 *
 * This software may be modified and distributed under the terms of the
 * BSD 2-clause license. See the LICENSE file for details.
 **************************************************************************/

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"path"
	"runtime"
	"runtime/pprof"
	"sync"
	"syscall"
	"time"

	"github.com/gravwell/gravwell/v3/ingest"
	"github.com/gravwell/gravwell/v3/ingest/entry"
	"github.com/gravwell/gravwell/v3/ingest/log"
	"github.com/gravwell/gravwell/v3/ingesters/utils"
	"github.com/gravwell/gravwell/v3/ingesters/version"
	"github.com/gravwell/gravwell/v3/timegrinder"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/kinesis"
)

const (
	defaultConfigLoc = `/opt/gravwell/etc/kinesis_ingest.conf`
)

var (
	configLoc      = flag.String("config-file", defaultConfigLoc, "Location of configuration file")
	verbose        = flag.Bool("v", false, "Display verbose status updates to stdout")
	ver            = flag.Bool("version", false, "Print the version information and exit")
	stderrOverride = flag.String("stderr", "", "Redirect stderr to a shared memory file")
	cpuprofile     = flag.String("cpuprofile", "", "write cpu profile to file")
	memprofile     = flag.String("memprofile", "", "File to write memory profile data to.  Disabled if blank")
	lg             *log.Logger
)

func init() {
	flag.Parse()
	if *ver {
		version.PrintVersion(os.Stdout)
		ingest.PrintVersion(os.Stdout)
		os.Exit(0)
	}
	lg = log.New(os.Stderr) // DO NOT close this, it will prevent backtraces from firing
	if *stderrOverride != `` {
		if oldstderr, err := syscall.Dup(int(os.Stderr.Fd())); err != nil {
			lg.Fatal("Failed to dup stderr: %v\n", err)
		} else {
			lg.AddWriter(os.NewFile(uintptr(oldstderr), "oldstderr"))
		}

		fp := path.Join(`/dev/shm/`, *stderrOverride)
		fout, err := os.Create(fp)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to create %s: %v\n", fp, err)
		} else {
			version.PrintVersion(fout)
			ingest.PrintVersion(fout)
			//file created, dup it
			if err := syscall.Dup2(int(fout.Fd()), int(os.Stderr.Fd())); err != nil {
				fmt.Fprintf(os.Stderr, "Failed to dup2 stderr: %v\n", err)
				fout.Close()
			}
		}
	}
}

func main() {
	if *cpuprofile != "" {
		f, err := os.Create(*cpuprofile)
		if err != nil {
			lg.Fatal("%v", err)
		}
		pprof.StartCPUProfile(f)
		defer pprof.StopCPUProfile()
	}
	if *memprofile != "" {
		defer func() {
			f, err := os.Create(*memprofile)
			if err != nil {
				fmt.Println("Failed to create memory profile", err)
				return
			}
			runtime.GC()
			pprof.WriteHeapProfile(f)
			f.Close()
		}()
	}

	var wg sync.WaitGroup
	running := true

	cfg, err := GetConfig(*configLoc)
	if err != nil {
		lg.Fatal("Failed to get configuration: %v", err)
	}
	if len(cfg.Global.Log_File) > 0 {
		fout, err := os.OpenFile(cfg.Global.Log_File, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0640)
		if err != nil {
			lg.FatalCode(0, "Failed to open log file %s: %v", cfg.Global.Log_File, err)
		}
		defer fout.Close()
		if err = lg.AddWriter(fout); err != nil {
			lg.Fatal("Failed to add a writer: %v", err)
		}
		if len(cfg.Global.Log_Level) > 0 {
			if err = lg.SetLevelString(cfg.Global.Log_Level); err != nil {
				lg.FatalCode(0, "Invalid Log Level \"%s\": %v", cfg.Global.Log_Level, err)
			}
		}
	}

	// Get the state file
	stateFile, err := utils.NewState(cfg.Global.State_Store_Location, 0600)
	if err != nil {
		lg.Fatal("Couldn't open state file: %v", err)
	}
	stateMan := NewStateman(stateFile)
	stateMan.Start()
	defer stateMan.Close()

	tags, err := cfg.Tags()
	if err != nil {
		lg.Fatal("Failed to get tags from configuration: %v", err)
	}
	conns, err := cfg.Targets()
	if err != nil {
		lg.Fatal("Failed to get backend targets from configuration: %s", err)
	}
	debugout("Handling %d tags over %d targets\n", len(tags), len(conns))

	//fire up the ingesters
	id, ok := cfg.Global.IngesterUUID()
	if !ok {
		lg.FatalCode(0, "Couldn't read ingester UUID\n")
	}
	ingestConfig := ingest.UniformMuxerConfig{
		IngestStreamConfig: cfg.Global.IngestStreamConfig,
		Destinations:       conns,
		Tags:               tags,
		Auth:               cfg.Secret(),
		LogLevel:           cfg.LogLevel(),
		Logger:             lg,
		IngesterName:       "Kinesis",
		IngesterVersion:    version.GetVersion(),
		IngesterUUID:       id.String(),
		CacheDepth:         cfg.Global.Cache_Depth,
		CachePath:          cfg.Global.Ingest_Cache_Path,
		CacheSize:          cfg.Global.Max_Ingest_Cache,
		CacheMode:          cfg.Global.Cache_Mode,
	}
	igst, err := ingest.NewUniformMuxer(ingestConfig)
	if err != nil {
		lg.Fatal("Failed build our ingest system: %v", err)
	}
	defer igst.Close()
	debugout("Starting ingester muxer\n")
	if err := igst.Start(); err != nil {
		lg.FatalCode(0, "Failed start our ingest system: %v", err)
		return
	}

	debugout("Waiting for connections to indexers ... ")
	if err := igst.WaitForHot(cfg.Timeout()); err != nil {
		lg.FatalCode(0, "Timedout waiting for backend connections: %v\n", err)
	}
	debugout("Successfully connected to ingesters\n")

	// Set up environment variables for AWS auth, if extant
	if cfg.Global.AWS_Access_Key_ID != "" {
		os.Setenv("AWS_ACCESS_KEY_ID", cfg.Global.AWS_Access_Key_ID)
	}
	if cfg.Global.AWS_Secret_Access_Key != "" {
		os.Setenv("AWS_SECRET_ACCESS_KEY", cfg.Global.AWS_Secret_Access_Key)
	}

	// make an aws session
	sess := session.Must(session.NewSession())

	dieChan := make(chan bool)

	ctx, cancel := context.WithCancel(context.Background())

	for _, stream := range cfg.KinesisStream {
		tagid, err := igst.GetTag(stream.Tag_Name)
		if err != nil {
			lg.Fatal("Can't resolve tag %v: %v", stream.Tag_Name, err)
		}

		// get a handle on kinesis
		svc := kinesis.New(sess, aws.NewConfig().WithRegion(stream.Region))

		// Get the list of shards
		shards := []*kinesis.Shard{}
		dsi := &kinesis.DescribeStreamInput{}
		dsi.SetStreamName(stream.Stream_Name)
		for {
			streamdesc, err := svc.DescribeStream(dsi)
			if err != nil {
				lg.Error("Failed to get stream description: %v", err)
				continue
			}
			newshards := streamdesc.StreamDescription.Shards
			shards = append(shards, newshards...)
			if *streamdesc.StreamDescription.HasMoreShards {
				dsi.SetExclusiveStartShardId(*(newshards[len(newshards)-1].ShardId))
			} else {
				break
			}
		}
		debugout("Read %d shards from stream %s\n", len(shards), stream.Stream_Name)

		// Now start up the metrics reporter
		var metricsTrackers []*shardMetrics
		if stream.Metrics_Interval > 0 {
			go func(stream streamDef) {
				for {
					select {
					case <-dieChan:
						return
					case <-time.After(time.Duration(stream.Metrics_Interval) * time.Second):
						report := metricsReport{StreamName: stream.Stream_Name, ShardCount: len(shards)}
						for i := range metricsTrackers {
							l, b, e, r := metricsTrackers[i].ReadAndReset()
							report.AverageLag += l
							report.CompressedDataSize += b
							report.EntryDataSize += e
							report.KinesisRequests += r
						}
						report.AverageLag = report.AverageLag / int64(len(metricsTrackers))
						if stream.JSON_Metrics {
							jr, err := json.Marshal(report)
							if err == nil {
								igst.Info("%v", string(jr))
							}
						} else {
							igst.Info("Stream %v: %v shards, avg %v ms behind latest. Since last update: %v compressed bytes read in %v requests, %v bytes uncompressed & processed", stream.Stream_Name, len(shards), report.AverageLag, report.CompressedDataSize, report.KinesisRequests, report.EntryDataSize)
						}
					}
				}
			}(*stream)
		}

		for i, shard := range shards {
			// Detect and skip closed shards
			if shard.SequenceNumberRange != nil && shard.SequenceNumberRange.EndingSequenceNumber != nil {
				lg.Info("Shard %v on stream %s appears to be closed, skipping", *shard.ShardId, stream.Stream_Name)
				continue
			}
			go func(stream streamDef, shard kinesis.Shard, tagid entry.EntryTag, shardid int) {
				wg.Add(1)
				defer wg.Done()

				// set up timegrinder and other long-lived stuff
				tcfg := timegrinder.Config{
					EnableLeftMostSeed: true,
				}
				tg, err := timegrinder.NewTimeGrinder(tcfg)
				if err != nil {
					stream.Parse_Time = false
				}
				if stream.Assume_Local_Timezone {
					tg.SetLocalTime()
				}
				if stream.Timezone_Override != `` {
					err = tg.SetTimezone(stream.Timezone_Override)
					if err != nil {
						fmt.Fprintf(os.Stderr, "Failed to set timezone to %v: %v\n", stream.Timezone_Override, err)
						return
					}
				}
				var src net.IP
				if cfg.Global.Source_Override != `` {
					// global override
					src = net.ParseIP(cfg.Global.Source_Override)
					if src == nil {
						lg.Fatal("Global Source-Override is invalid")
					}
				}

				// one processor set per shard
				procset, err := cfg.Preprocessor.ProcessorSet(igst, stream.Preprocessor)
				if err != nil {
					lg.Fatal("Preprocessor construction error: %v", err)
				}

				// make the shardMetrics and add it to the array
				tracker := shardMetrics{}
				metricsTrackers = append(metricsTrackers, &tracker)
				if stream.Metrics_Interval == 0 {
					// disable it
					tracker.Disabled = true
				}

			reconnectLoop:
				for {
					gsii := &kinesis.GetShardIteratorInput{}
					gsii.SetShardId(*shard.ShardId)
					gsii.SetStreamName(stream.Stream_Name)
					seqnum := stateMan.GetSequenceNum(stream.Stream_Name, *shard.ShardId)
					if seqnum == `` {
						// we don't have a previous state
						debugout("No previous sequence number for stream %v shard %v, defaulting to %v\n", stream.Stream_Name, *shard.ShardId, stream.Iterator_Type)
						gsii.SetShardIteratorType(stream.Iterator_Type)
					} else {
						gsii.SetShardIteratorType(`AFTER_SEQUENCE_NUMBER`)
						gsii.SetStartingSequenceNumber(seqnum)
					}

					output, err := svc.GetShardIterator(gsii)
					if err != nil {
						lg.Error("error on shard #%d (%s): %v", shardid, *shard.ShardId, err)
						time.Sleep(5 * time.Second)
						continue
					}
					if output.ShardIterator == nil {
						// this is weird, we are going to bail out
						lg.Error("Got nil initial shard iterator, sleeping and retrying")
						time.Sleep(5 * time.Second)
						continue
					}
					iter := *output.ShardIterator

					var lastSeqNum string
					for running {
						gri := &kinesis.GetRecordsInput{}
						gri.SetLimit(5000)
						gri.SetShardIterator(iter)
						var res *kinesis.GetRecordsOutput
						var err error
						for {
							res, err = svc.GetRecords(gri)
							if res != nil {
								if res.NextShardIterator != nil {
									iter = *res.NextShardIterator
								}
							}
							if err != nil {
								if awsErr, ok := err.(awserr.Error); ok {
									// process SDK error
									if awsErr.Code() == kinesis.ErrCodeProvisionedThroughputExceededException {
										lg.Warn("Throughput exceeded, trying again")
										time.Sleep(500 * time.Millisecond)
									} else if awsErr.Code() == kinesis.ErrCodeExpiredIteratorException {
										lg.Info("Iterator expired, re-initializing")
										time.Sleep(100 * time.Millisecond)
										continue reconnectLoop
									} else {
										lg.Error("%s: %s", awsErr.Code(), awsErr.Message())
										time.Sleep(500 * time.Millisecond)
									}
								} else {
									lg.Error("unknown error: %v", err)
								}
							} else {
								// if we got no records, chill for a sec before we hit it again
								if len(res.Records) == 0 {
									time.Sleep(100 * time.Millisecond)
								}
								break
							}
						}

						var entrySize int
						for _, r := range res.Records {
							lastSeqNum = *r.SequenceNumber
							ent := &entry.Entry{
								Tag:  tagid,
								SRC:  src,
								Data: r.Data,
							}
							if stream.Parse_Time == false {
								ent.TS = entry.FromStandard(*r.ApproximateArrivalTimestamp)
							} else {
								ts, ok, err := tg.Extract(ent.Data)
								if !ok || err != nil {
									// something went wrong, switch to using kinesis timestamps
									stream.Parse_Time = false
									ent.TS = entry.FromStandard(*r.ApproximateArrivalTimestamp)
								} else {
									ent.TS = entry.FromStandard(ts)
								}
							}
							if err = procset.ProcessContext(ent, ctx); err != nil {
								lg.Error("Failed to handle entry: %v", err)
							}
							entrySize += int(ent.Size())
						}
						tracker.Update(res, entrySize)
						// Now update the most recent sequence number
						if lastSeqNum != `` {
							stateMan.UpdateSequenceNum(stream.Stream_Name, *shard.ShardId, lastSeqNum)
						}
					}
					if err = procset.Close(); err != nil {
						lg.Error("Failed to close processor set: %v", err)
					}
					// if we get to this point, exit the for loop
					break
				}
			}(*stream, *shard, tagid, i)
		}
	}

	utils.WaitForQuit()

	running = false
	close(dieChan)

	go func() {
		time.Sleep(time.Second)
		cancel()
	}()

	wg.Wait()
}

func debugout(format string, args ...interface{}) {
	if !*verbose {
		return
	}
	fmt.Printf(format, args...)
}

type metricsReport struct {
	StreamName         string
	ShardCount         int
	AverageLag         int64
	CompressedDataSize uint64
	EntryDataSize      uint64
	KinesisRequests    uint64
}

type shardMetrics struct {
	sync.Mutex
	Disabled     bool
	millisbehind int64
	datasize     uint64
	entrysize    uint64
	requests     uint64
}

func (s *shardMetrics) Update(res *kinesis.GetRecordsOutput, entrySize int) {
	s.Lock()
	defer s.Unlock()
	if s.Disabled {
		return
	}
	s.millisbehind = *res.MillisBehindLatest
	s.requests++
	var dsize int
	for i := range res.Records {
		dsize += len(res.Records[i].Data)
	}
	s.datasize += uint64(dsize)
	s.entrysize += uint64(entrySize)
}

func (s *shardMetrics) ReadAndReset() (millis int64, datasize uint64, entrysize uint64, requests uint64) {
	s.Lock()
	defer s.Unlock()
	millis = s.millisbehind
	datasize = s.datasize
	s.datasize = 0
	entrysize = s.entrysize
	s.entrysize = 0
	requests = s.requests
	s.requests = 0
	return
}

type stateman struct {
	sync.Mutex
	states    map[string]map[string]string // map of stream name to shard name to sequence number
	stateFile *utils.State
}

func NewStateman(stateFile *utils.State) *stateman {
	sm := stateman{
		states:    make(map[string]map[string]string),
		stateFile: stateFile,
	}
	stateFile.Read(&sm.states)
	return &sm
}

func (s *stateman) Start() {
	go func() {
		for {
			select {
			case <-time.After(15 * time.Second):
				s.Flush()
			}
		}
	}()
}

func (s *stateman) Close() {
	s.Flush()
}

func (s *stateman) Flush() {
	s.Lock()
	defer s.Unlock()
	s.stateFile.Write(s.states)
}

func (s *stateman) UpdateSequenceNum(stream, shard, seq string) {
	s.Lock()
	defer s.Unlock()

	_, ok := s.states[stream]
	if !ok {
		// initialize the stream
		s.states[stream] = make(map[string]string)
	}
	s.states[stream][shard] = seq
}

func (s *stateman) GetSequenceNum(stream, shard string) string {
	s.Lock()
	defer s.Unlock()

	_, ok := s.states[stream]
	if !ok {
		// initialize the stream
		s.states[stream] = make(map[string]string)
	}
	return s.states[stream][shard]
}
