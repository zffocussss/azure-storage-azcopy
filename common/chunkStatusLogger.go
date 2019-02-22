// Copyright © 2017 Microsoft <wastore@microsoft.com>
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package common

import (
	"bufio"
	"fmt"
	"os"
	"path"
	"sync/atomic"
	"time"
)

// Identifies a chunk. Always create with NewChunkID
type ChunkID struct {
	Name         string
	OffsetInFile int64

	// What is this chunk's progress currently waiting on?
	// Must be a pointer, because the ChunkID itself is a struct.
	// When chunkID is passed around, copies are made,
	// but because this is a pointer, all will point to the same
	// value for waitReasonIndex (so when we change it, all will see the change)
	waitReasonIndex *int32
}

func NewChunkID(name string, offsetInFile int64) ChunkID {
	dummyWaitReasonIndex := int32(0)
	return ChunkID{
		Name:            name,
		OffsetInFile:    offsetInFile,
		waitReasonIndex: &dummyWaitReasonIndex, // must initialize, so don't get nil pointer on usage
	}
}

var EWaitReason = WaitReason{0, ""}

// WaitReason identifies the one thing that a given chunk is waiting on, at a given moment.
// Basically = state, phrased in terms of "the thing I'm waiting for"
type WaitReason struct {
	index int32
	Name  string
}

// Head (below) has index between GB and Body, just so the ordering is numerical ascending during typical chunk lifetime for both upload and download
func (WaitReason) Nothing() WaitReason              { return WaitReason{0, "Nothing"} }            // not waiting for anything
func (WaitReason) RAMToSchedule() WaitReason        { return WaitReason{1, "RAM"} }                // waiting for enough RAM to schedule the chunk
func (WaitReason) WorkerGR() WaitReason             { return WaitReason{2, "Worker"} }             // waiting for a goroutine to start running our chunkfunc
func (WaitReason) HeaderResponse() WaitReason       { return WaitReason{3, "Head"} }               // waiting to finish downloading the HEAD
func (WaitReason) Body() WaitReason                 { return WaitReason{4, "Body"} }               // waiting to finish sending/receiving the BODY
func (WaitReason) BodyReReadDueToMem() WaitReason   { return WaitReason{5, "BodyReRead-LowRam"} }  //waiting to re-read the body after a forced-retry due to low RAM
func (WaitReason) BodyReReadDueToSpeed() WaitReason { return WaitReason{6, "BodyReRead-TooSlow"} } // waiting to re-read the body after a forced-retry due to a slow chunk read (without low RAM)
func (WaitReason) Sorting() WaitReason              { return WaitReason{7, "Sorting"} }            // waiting for the writer routine, in chunkedFileWriter, to pick up this chunk and sort it into sequence
func (WaitReason) PriorChunk() WaitReason           { return WaitReason{8, "Prior"} }              // waiting on a prior chunk to arrive (before this one can be saved)
func (WaitReason) QueueToWrite() WaitReason         { return WaitReason{9, "Queue"} }              // prior chunk has arrived, but is not yet written out to disk
func (WaitReason) DiskIO() WaitReason               { return WaitReason{10, "DiskIO"} }            // waiting on disk read/write to complete
func (WaitReason) ChunkDone() WaitReason            { return WaitReason{11, "Done"} }              // not waiting on anything. Chunk is done.
func (WaitReason) Cancelled() WaitReason            { return WaitReason{12, "Cancelled"} }         // transfer was cancelled.  All chunks end with either Done or Cancelled.

// TODO: consider change the above so that they don't create new struct on every call?  Is that necessary/useful?
//     Note: reason it's not using the normal enum approach, where it only has a number, is to try to optimize
//     the String method below, on the assumption that it will be called a lot.  Is that a premature optimization?

// Upload chunks go through these states, in this order.
// We record this set of states, in this order, so that when we are uploading GetCounts() can return
// counts for only those states that are relevant to upload (some are not relevant, so they are not in this list)
// AND so that GetCounts will return the counts in the order that the states actually happen when uploading.
// That makes it easy for end-users of the counts (i.e. logging and display code) to show the state counts
// in a meaningful left-to-right sequential order.
var uploadWaitReasons = []WaitReason{
	// These first two happen in the transfer initiation function (i.e. the chunkfunc creation loop)
	// So their total is constrained to the size of the goroutine pool that runs those functions.
	// (e.g. 64, given the GR pool sizing as at Feb 2019)
	EWaitReason.RAMToSchedule(),
	EWaitReason.DiskIO(),

	// This next one is used when waiting for a worker Go routine to pick up the scheduled chunk func.
	// Chunks in this state are effectively a queue of work waiting to be sent over the network
	EWaitReason.WorkerGR(),

	// This is the actual network activity
	EWaitReason.Body(), // header is not separated out for uploads, so is implicitly included here
	// Plus Done/cancelled, which are not included here because not wanted for GetCounts
}

// Download chunks go through a larger set of states, due to needing to be re-assembled into sequential order
// See comment on uploadWaitReasons for rationale.
var downloadWaitReasons = []WaitReason{
	// Done by the transfer initiation function (i.e. chunkfunc creation loop)
	EWaitReason.RAMToSchedule(),

	// Waiting for a work Goroutine to pick up the chunkfunc and execute it.
	// Chunks in this state are effectively a queue of work, waiting for their network downloads to be initiated
	EWaitReason.WorkerGR(),

	// These next ones are the actual network activity
	EWaitReason.HeaderResponse(),
	EWaitReason.Body(),
	// next two exist, but are not reported on separately in GetCounts, so are commented out
	//EWaitReason.BodyReReadDueToMem(),
	//EWaitReason.BodyReReadDueToSpeed(),

	// Sorting and QueueToWrite together comprise a queue of work waiting to be written to disk.
	// The former are unsorted, and the latter have been sorted into sequential order.
	// PriorChunk is unusual, because chunks in that wait state are not (yet) waiting for their turn to be written to disk,
	// instead they are waiting on some prior chunk to finish arriving over the network
	EWaitReason.Sorting(),
	EWaitReason.PriorChunk(),
	EWaitReason.QueueToWrite(),

	// The actual disk write
	EWaitReason.DiskIO(),
	// Plus Done/cancelled, which are not included here because not wanted for GetCounts
}

func (wr WaitReason) String() string {
	return string(wr.Name) // avoiding reflection here, for speed, since will be called a lot
}

type ChunkStatusLogger interface {
	LogChunkStatus(id ChunkID, reason WaitReason)
}

type ChunkStatusLoggerCloser interface {
	ChunkStatusLogger
	GetCounts(isDownload bool) []chunkStatusCount
	IsDiskConstrained(isUpload, isDownload bool) bool
	CloseLog()
}

// chunkStatusLogger records all chunk state transitions, and makes aggregate data immediately available
// for performance diagnostics. Also optionally logs every individual transition to a file.
type chunkStatusLogger struct {
	counts         []int64
	outputEnabled  bool
	unsavedEntries chan chunkWaitState
}

func NewChunkStatusLogger(jobID JobID, logFileFolder string, enableOutput bool) ChunkStatusLoggerCloser {
	logger := &chunkStatusLogger{
		counts:         make([]int64, numWaitReasons()),
		outputEnabled:  enableOutput,
		unsavedEntries: make(chan chunkWaitState, 1000000),
	}
	if enableOutput {
		chunkLogPath := path.Join(logFileFolder, jobID.String()+"-chunks.log") // its a CSV, but using log extension for consistency with other files in the directory
		go logger.main(chunkLogPath)
	}
	return logger
}

func numWaitReasons() int32 {
	return EWaitReason.Cancelled().index + 1 // assume this is the last wait reason
}

type chunkStatusCount struct {
	WaitReason WaitReason
	Count      int64
}

type chunkWaitState struct {
	ChunkID
	reason    WaitReason
	waitStart time.Time
}

////////////////////////////////////  basic functionality //////////////////////////////////

func (csl *chunkStatusLogger) LogChunkStatus(id ChunkID, reason WaitReason) {
	// always update the in-memory stats, even if output is disabled
	csl.countStateTransition(id, reason)

	if !csl.outputEnabled {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			// recover panic from writing to closed channel
			// May happen in early exit of app, when Close is called before last call to this routine
		}
	}()

	csl.unsavedEntries <- chunkWaitState{ChunkID: id, reason: reason, waitStart: time.Now()}
}

func (csl *chunkStatusLogger) CloseLog() {
	if !csl.outputEnabled {
		return
	}
	close(csl.unsavedEntries)
	for len(csl.unsavedEntries) > 0 {
		time.Sleep(100 * time.Millisecond)
	}
}

func (csl *chunkStatusLogger) main(chunkLogPath string) {
	f, err := os.Create(chunkLogPath)
	if err != nil {
		panic(err.Error())
	}
	defer func() { _ = f.Close() }()

	w := bufio.NewWriter(f)
	defer func() { _ = w.Flush() }()

	_, _ = w.WriteString("Name,Offset,State,StateStartTime\n")

	for x := range csl.unsavedEntries {
		_, _ = w.WriteString(fmt.Sprintf("%s,%d,%s,%s\n", x.Name, x.OffsetInFile, x.reason, x.waitStart))
	}
}

////////////////////////////// aggregate count and analysis support //////////////////////

// We maintain running totals of how many chunks are in each state.
// To do so, we must determine the new state (which is simply a parameter) and the old state.
// We obtain and track the old state within the chunkID itself. The alternative, of having a threadsafe
// map in the chunkStatusLogger, to track and look up the states, is considered a risk for performance.
func (csl *chunkStatusLogger) countStateTransition(id ChunkID, newReason WaitReason) {

	// Flip the chunk's state to indicate the new thing that it's waiting for now
	oldReasonIndex := atomic.SwapInt32(id.waitReasonIndex, newReason.index)

	// Update the counts
	// There's no need to lock the array itself. Instead just do atomic operations on the contents.
	// (See https://groups.google.com/forum/#!topic/Golang-nuts/Ud4Dqin2Shc)
	if oldReasonIndex > 0 && oldReasonIndex < int32(len(csl.counts)) {
		atomic.AddInt64(&csl.counts[oldReasonIndex], -1)
	}
	if newReason.index < int32(len(csl.counts)) {
		atomic.AddInt64(&csl.counts[newReason.index], 1)
	}
}

func (csl *chunkStatusLogger) getCount(reason WaitReason) int64 {
	return atomic.LoadInt64(&csl.counts[reason.index])
}

// Gets the current counts of chunks in each wait state
// Intended for performance diagnostics and reporting
func (csl *chunkStatusLogger) GetCounts(isDownload bool) []chunkStatusCount {

	var allReasons []WaitReason
	if isDownload {
		allReasons = downloadWaitReasons
	} else {
		allReasons = uploadWaitReasons
	}

	result := make([]chunkStatusCount, len(allReasons))
	for i, reason := range allReasons {
		count := csl.getCount(reason)

		// for simplicity in consuming the results, all the body read states are rolled into one here
		if reason == EWaitReason.BodyReReadDueToSpeed() || reason == EWaitReason.BodyReReadDueToMem() {
			panic("body re-reads should not be requested in counts. They get rolled into the main Body one")
		}
		if reason == EWaitReason.Body() {
			count += csl.getCount(EWaitReason.BodyReReadDueToSpeed())
			count += csl.getCount(EWaitReason.BodyReReadDueToMem())
		}

		result[i] = chunkStatusCount{reason, count}
	}
	return result
}

func (csl *chunkStatusLogger) IsDiskConstrained(isUpload, isDownload bool) bool {
	if isUpload {
		return csl.isUploadDiskConstrained()
	} else if isDownload {
		return csl.isDownloadDiskConstrained()
	} else {
		return false // it's neither upload nor download (e.g. S2S)
	}
}

// is disk the bottleneck in an upload?
func (csl *chunkStatusLogger) isUploadDiskConstrained() bool {
	// If we are uploading, and there's almost nothing waiting to go out over the network, then
	// probably the reason there's not much queued is that the disk is slow.
	// BTW, we can't usefully look at any of the _earlier_ states, because they happen in the _generation_ of the chunk funcs
	// (not the _execution_ and so their counts will just tend to equal that of the small goroutine pool that runs them).
	// It might be convenient if we could compare TWO queue sizes here, as we do in isDownloadDiskConstrained, but unfortunately our
	// Jan 2019 architecture only gives us ONE useful queue-like state when uploading, so we can't compare two.
	const nearZeroQueueSize = 10 // TODO: is there any intelligent way to set this threshold? It's just an arbitrary guestimate of "small" at the moment
	queueForNetworkIsSmall := csl.getCount(EWaitReason.WorkerGR()) < nearZeroQueueSize

	beforeGRWaitQueue := csl.getCount(EWaitReason.RAMToSchedule()) + csl.getCount(EWaitReason.DiskIO())
	areStillReadingDisk := beforeGRWaitQueue > 0 // size of queue for network is irrelevant if we are no longer actually reading disk files, and therefore no longer putting anything into the queue for network

	return areStillReadingDisk && queueForNetworkIsSmall
}

// is disk the bottleneck in a download?
func (csl *chunkStatusLogger) isDownloadDiskConstrained() bool {
	// See how many chunks are waiting on the disk. I.e. are queued before the actual disk state.
	// Don't include the "PriorChunk" state, because that's not actually waiting on disk at all, it
	// can mean waiting on network and/or waiting-on-Storage-Service. We don't know which. So we just exclude it from consideration.
	chunksWaitingOnDisk := csl.getCount(EWaitReason.Sorting()) + csl.getCount(EWaitReason.QueueToWrite())

	// i.e. are queued before the actual network states
	chunksWaitingOnNetwork := csl.getCount(EWaitReason.WorkerGR())

	// if we have way more stuff waiting on disk than on network, we can assume disk is the bottleneck
	const activeDiskQThreshold = 10
	const bigDifference = 5                              // TODO: review/tune the arbitrary constant here
	return chunksWaitingOnDisk > activeDiskQThreshold && // this test is in case both are near zero, as they would be near the end of the job
		chunksWaitingOnDisk > bigDifference*chunksWaitingOnNetwork
}

///////////////////////////////////// Sample LinqPad query for manual analysis of chunklog /////////////////////////////////////

/* LinqPad query used to analyze/visualize the CSV as is follows:
   Needs CSV driver for LinqPad to open the CSV - e.g. https://github.com/dobrou/CsvLINQPadDriver

var data = chunkwaitlog_noForcedRetries;

const int assumedMBPerChunk = 8;

DateTime? ParseStart(string s)
{
	const string format = "yyyy-MM-dd HH:mm:ss.fff";
	var s2 = s.Substring(0, format.Length);
	try
	{
		return DateTime.ParseExact(s2, format, CultureInfo.CurrentCulture);
	}
	catch
	{
		return null;
	}
}

// convert to real datetime (default unparseable ones to a fixed value, simply to avoid needing to deal with nulls below, and because all valid records should be parseable. Only exception would be something partially written a time of a crash)
var parsed = data.Select(d => new { d.Name, d.Offset, d.State, StateStartTime = ParseStart(d.StateStartTime) ?? DateTime.MaxValue}).ToList();

var grouped = parsed.GroupBy(c => new {c.Name, c.Offset});

var statesForOffset = grouped.Select(g => new
{
	g.Key,
	States = g.Select(x => new { x.State, x.StateStartTime }).OrderBy(x => x.StateStartTime).ToList()
}).ToList();

var withStatesOfInterest = (from sfo in statesForOffset
let states = sfo.States
let lastIndex = states.Count - 1
let statesWithDurations = states.Select((s, i) => new{ s.State, s.StateStartTime, Duration = ( i == lastIndex ? new TimeSpan(0) : states[i+1].StateStartTime - s.StateStartTime) })
let hasLongBodyRead = statesWithDurations.Any(x => (x.State == "Body" && x.Duration.TotalSeconds > 30)  // detect slowness in tests where we turn off the forced restarts
|| x.State.StartsWith("BodyReRead"))                       // detect slowness where we solved it by a forced restart
select new {sfo.Key, States = statesWithDurations, HasLongBodyRead = hasLongBodyRead})
.ToList();

var filesWithLongBodyReads = withStatesOfInterest.Where(x => x.HasLongBodyRead).Select(x => x.Key.Name).Distinct().ToList();

filesWithLongBodyReads.Count().Dump("Number of files with at least one long chunk read");

var final = (from wsi in withStatesOfInterest
join f in filesWithLongBodyReads on wsi.Key.Name equals f
select new
{
ChunkID = wsi.Key,
wsi.HasLongBodyRead,
wsi.States

})
.GroupBy(f => f.ChunkID.Name)
.Select(g => new {
Name = g.Key,
Chunks = g.Select(x => new {
OffsetNumber = (int)(long.Parse(x.ChunkID.Offset)/(assumedMBPerChunk*1024*1024)),
OffsetValue = x.HasLongBodyRead ? Util.Highlight(x.ChunkID.Offset) : x.ChunkID.Offset, States = x.States}
).OrderBy(x => x.OffsetNumber)
})
.OrderBy(x => x.Name);

final.Dump();

*/