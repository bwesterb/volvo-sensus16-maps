package main

import (
	"cmp"
	"context"
	"crypto/md5"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	maxWorkers   = 12
	retries      = 8
	stallTimeout = 30 * time.Second // without progress on a connection
	window       = 2 * maxWorkers   // chunks buffered ahead of the writer
)

// Concurrency adapts the number of parallel requests to what the link and
// disk take. Every period:
//   - halve after a burst of errors (be kind to the servers);
//   - drop one (to no fewer than two) if the disk can't keep up, i.e. the
//     loop handing out requests often has to wait for the writer;
//   - otherwise probe: one more, kept only if throughput clearly improves;
//     if it doesn't, one fewer, kept unless throughput clearly drops (fewer
//     connections for the same speed is nicer to the servers). Then hold
//     for a while before probing again.
type Concurrency struct {
	mu       sync.Mutex
	limit    int
	adaptive bool
	t0       time.Time
	net0     int64
	wait     time.Duration // request loop waiting for the writer
	requests int
	errors   int
	lastRate float64
	probing  string // "", "up" or "down"
	hold     int
}

const (
	ccPeriod    = 5 * time.Second
	ccHold      = 6    // periods to wait after probing
	ccGain      = 1.10 // throughput change that counts as real, not noise
	ccDiskBound = 0.3  // request loop waits for the writer more than this fraction
)

func newConcurrency(fixed int) *Concurrency {
	c := &Concurrency{limit: 4, adaptive: fixed == 0, t0: time.Now()}
	if fixed != 0 {
		c.limit = fixed
	}
	return c
}

func (c *Concurrency) Limit() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.limit
}

func (c *Concurrency) request(failed bool) {
	c.mu.Lock()
	c.requests++
	if failed {
		c.errors++
	}
	c.mu.Unlock()
}

func (c *Concurrency) blockedOnDisk(d time.Duration) {
	c.mu.Lock()
	c.wait += d
	c.mu.Unlock()
}

func (c *Concurrency) tick(net int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	dt := now.Sub(c.t0)
	if dt < ccPeriod {
		return
	}
	rate := float64(net-c.net0) / dt.Seconds()
	requests, errs := c.requests, c.errors
	diskBound := c.wait.Seconds()/dt.Seconds() > ccDiskBound
	c.t0, c.net0, c.wait, c.requests, c.errors = now, net, 0, 0, 0
	if !c.adaptive {
		return
	}
	switch {
	case errs >= 3 && float64(errs) >= 0.1*float64(requests):
		c.limit = max(1, c.limit/2)
		c.probing, c.hold = "", ccHold
	case diskBound:
		// More connections won't help, but keep two: a single connection
		// can be slower than the disk's average.
		c.limit = max(min(2, c.limit), c.limit-1)
		c.probing, c.hold = "", ccHold
	case c.probing == "up":
		if rate >= c.lastRate*ccGain {
			c.probing = "" // helped: probe again next time
		} else {
			c.limit--
			if c.limit > 1 {
				c.limit--
				c.probing = "down"
			} else {
				c.probing, c.hold = "", ccHold
			}
			rate = c.lastRate // compare the down probe to the baseline
		}
	case c.probing == "down":
		if rate*ccGain < c.lastRate {
			c.limit++ // fewer hurt: go back
		}
		c.probing, c.hold = "", ccHold
	case errs > 0 || c.hold > 0:
		c.hold = max(0, c.hold-1)
	case c.limit < maxWorkers:
		c.limit++
		c.probing = "up"
	}
	c.lastRate = rate
}

type nodeStat struct {
	rtt      time.Duration
	failures int // in a row
	bytes    int64
	active   atomic.Int64 // requests running now (for display)
}

// Downloader fetches files chunk by chunk from the nodes.
//
// It uses one node at a time: the nearest. A single node can fill a fast
// home connection with a few connections, so spreading over nodes adds no
// capacity -- only competition at the local bottleneck, where connections
// to a more distant node (higher RTT) get a smaller share of the bandwidth
// and become stragglers that the in-order writer has to wait for. In tests
// all nodes at once were slower, and much less steady, than any single
// one. Per-request speed is no use for choosing either: it mostly reflects
// how many connections share the line.
type Downloader struct {
	nodes  []Node // nearest first
	cc     *Concurrency
	client *http.Client

	mu      sync.Mutex
	stats   map[string]*nodeStat
	reqTime float64 // EWMA of a successful request's duration, seconds

	inFlight atomic.Int64
	wake     chan struct{} // a request finished
	net      atomic.Int64  // bytes downloaded
	retries  atomic.Int64  // failed requests
	hedges   atomic.Int64  // extra requests for stragglers

	done       atomic.Int64 // bytes on disk, written or found present
	total      int64
	dest       string
	writeBytes atomic.Int64  // written to dest
	writeNanos atomic.Int64  // spent in writes and syncs
	diskWait   time.Duration // the loop handing out requests waited for the writer
	t0         time.Time
	st         status
}

func newDownloader(ctx context.Context, nodes []Node, cc *Concurrency) *Downloader {
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: stallTimeout,
		MaxIdleConnsPerHost:   maxWorkers + 1,
		IdleConnTimeout:       90 * time.Second,
		DisableCompression:    true,
		// HTTP/1.1 like the original: a connection per request in flight.
		TLSNextProto: map[string]func(string, *tls.Conn) http.RoundTripper{},
	}
	d := &Downloader{
		nodes:  slices.Clone(nodes),
		cc:     cc,
		client: &http.Client{Transport: tr},
		stats:  map[string]*nodeStat{},
		wake:   make(chan struct{}, 1),
		t0:     time.Now(),
		st:     newStatus(),
	}
	rtts := make([]time.Duration, len(nodes))
	var wg sync.WaitGroup
	for i, n := range nodes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rtts[i] = measureRTT(ctx, n.Host)
		}()
	}
	wg.Wait()
	for i, n := range nodes {
		d.stats[n.Host] = &nodeStat{rtt: rtts[i]}
	}
	slices.SortStableFunc(d.nodes, func(a, b Node) int {
		return cmp.Compare(d.stats[a.Host].rtt, d.stats[b.Host].rtt)
	})
	return d
}

// measureRTT is the best of three TCP connects to host's HTTPS port, or an
// hour if it can't be reached.
func measureRTT(ctx context.Context, host string) time.Duration {
	best := time.Hour
	var dialer net.Dialer
	for range 3 {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		t := time.Now()
		c, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(host, "443"))
		cancel()
		if err == nil {
			best = min(best, time.Since(t))
			c.Close()
		}
	}
	return best
}

// pickNode picks the nearest node that isn't excluded. A node that fails
// three times in a row moves to the back of the line.
func (d *Downloader) pickNode(exclude map[string]bool) Node {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, n := range d.nodes {
		if !exclude[n.Host] {
			return n
		}
	}
	return d.nodes[0]
}

// record notes a request's outcome: bytes served, or a failure (nbytes 0).
func (d *Downloader) record(host string, nbytes int, dur time.Duration) {
	d.mu.Lock()
	defer d.mu.Unlock()
	st := d.stats[host]
	if nbytes == 0 {
		st.failures++
		if st.failures >= 3 && d.nodes[0].Host == host && len(d.nodes) > 1 {
			d.nodes = append(d.nodes[1:], d.nodes[0])
			st.failures = 0
		}
		return
	}
	st.failures = 0
	st.bytes += int64(nbytes)
	if d.reqTime == 0 {
		d.reqTime = dur.Seconds()
	} else {
		d.reqTime = 0.8*d.reqTime + 0.2*dur.Seconds()
	}
}

func (d *Downloader) hedgeAfter() time.Duration {
	d.mu.Lock()
	rt := d.reqTime
	d.mu.Unlock()
	if rt == 0 {
		rt = 1
	}
	return max(2*time.Second, time.Duration(3*rt*float64(time.Second)))
}

// get fetches bytes [start, end] of urlPath from node n.
func (d *Downloader) get(ctx context.Context, n Node, urlPath string, start, end int64, got *atomic.Int64) ([]byte, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stall := time.AfterFunc(stallTimeout, cancel)
	defer stall.Stop()
	req, err := http.NewRequestWithContext(ctx, "GET", "https://"+n.Host+n.Path+urlPath, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
	resp, err := d.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case 206:
	case 200: // server ignored the range
		if _, err := io.CopyN(io.Discard, resp.Body, start); err != nil {
			return nil, err
		}
	default:
		// Read a short error page to the end so the connection can be reused.
		io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return nil, fmt.Errorf("HTTP %d from %s", resp.StatusCode, n.Host)
	}
	buf := make([]byte, end-start+1)
	off := 0
	sawEOF := false
	for off < len(buf) {
		m, err := resp.Body.Read(buf[off:])
		off += m
		got.Add(int64(m))
		stall.Reset(stallTimeout)
		if err == io.EOF {
			sawEOF = true
			break
		}
		if err != nil {
			return nil, err
		}
	}
	if off != len(buf) {
		return nil, fmt.Errorf("short read from %s", n.Host)
	}
	// The connection only goes back to the pool once the body has been read
	// to EOF. net/http reports that with the last bytes of a Content-Length
	// body, but make sure -- and that the server didn't send more.
	if !sawEOF && resp.StatusCode == 206 {
		var one [1]byte
		if m, _ := io.ReadFull(resp.Body, one[:]); m > 0 {
			return nil, fmt.Errorf("%s sent more than the requested range", n.Host)
		}
	}
	return buf, nil
}

// chunkJob is one chunk the writer is waiting for: a request, plus maybe a
// hedge request to another node if it straggles.
type chunkJob struct {
	results  chan chunkResult
	started  time.Time
	launched int
	failed   int
	host     atomic.Pointer[string] // node the first request is asking

	// For the status display.
	size    int64
	got     [2]atomic.Int64 // bytes received by the request and the hedge
	retried atomic.Bool
}

type chunkResult struct {
	data []byte
	err  error
}

// fetchChunk downloads chunk i of f and returns its verified bytes.
//
// The first attempt goes to home, which launch picked and already counts as
// active there; retries pick another node.
func (d *Downloader) fetchChunk(ctx context.Context, urlPath string, f File, i int64, crc string, home Node, failed map[string]bool, hedge bool, job *chunkJob, got *atomic.Int64) ([]byte, error) {
	start := i * f.Chunk
	end := min(start+f.Chunk, f.Size) - 1
	var lastErr error
	for attempt := 0; attempt < retries; attempt++ {
		if attempt > 0 {
			home = d.pickNode(failed)
			if !hedge {
				job.host.Store(&home.Host)
			}
			d.stats[home.Host].active.Add(1)
		}
		t := time.Now()
		got.Store(0)
		data, err := d.get(ctx, home, urlPath, start, end, got)
		d.stats[home.Host].active.Add(-1) // the map itself never changes
		if err == nil && crc != "" && fmt.Sprintf("%08X", crc32.ChecksumIEEE(data)) != strings.ToUpper(crc) {
			err = fmt.Errorf("checksum mismatch from %s", home.Host)
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if err == nil {
			d.record(home.Host, len(data), time.Since(t))
			d.net.Add(int64(len(data)))
			d.cc.request(false)
			return data, nil
		}
		// Try another node next time, and don't hammer: back off
		// exponentially with jitter (the original retries at once).
		d.record(home.Host, 0, time.Since(t))
		d.cc.request(true)
		d.retries.Add(1)
		failed[home.Host] = true
		lastErr = err
		job.retried.Store(true)
		backoff := time.Duration(min(1<<attempt, 30)) * time.Second
		backoff = time.Duration(float64(backoff) * (0.8 + 0.4*rand.Float64()))
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return nil, fmt.Errorf("%s chunk %d: giving up after %d tries: %w", f.Path, i, retries, lastErr)
}

func (d *Downloader) launch(ctx context.Context, urlPath string, f File, i int64, crc, avoid string, job *chunkJob) {
	got := &job.got[job.launched]
	job.launched++
	// Pick the node now rather than in the goroutine, so the status display
	// never shows a request as in flight without a server.
	failed := map[string]bool{}
	if avoid != "" {
		failed[avoid] = true
	}
	home := d.pickNode(failed)
	if avoid == "" {
		job.host.Store(&home.Host)
	}
	d.stats[home.Host].active.Add(1)
	d.inFlight.Add(1)
	go func() {
		data, err := d.fetchChunk(ctx, urlPath, f, i, crc, home, failed, avoid != "", job, got)
		d.inFlight.Add(-1)
		job.results <- chunkResult{data, err}
		select {
		case d.wake <- struct{}{}:
		default:
		}
	}()
}

// fetchFile downloads f into dest via a .part file, writing strictly in
// order. Chunks are fetched in parallel but written sequentially: USB
// sticks are noticeably faster at sequential writes, and on FAT extending
// a file past a gap zero-fills the gap first. The MD5 is computed over the
// bytes in order.
//
// Writing happens in its own goroutine, behind a queue: a single uncached
// 4 MiB write to a USB stick can take anything from tens of milliseconds
// to over a second, and if the loop that hands out requests also wrote,
// the network would sit idle during every write. Downloading and writing
// would take turns instead of overlapping, and the overall rate would drop
// well below both.
//
// With keepPart, the verified file stays a .part file for the caller to
// rename.
func (d *Downloader) fetchFile(ctx context.Context, m *Manifest, f File, dest string, keepPart bool) error {
	final := filepath.Join(dest, f.Path)
	part := final + ".part"
	n := (f.Size + f.Chunk - 1) / f.Chunk
	crcs := make([]string, n)
	if int64(len(f.Checksum)) == 8*n {
		for i := range crcs {
			crcs[i] = f.Checksum[8*i : 8*i+8]
		}
	} // else: unknown layout; rely on the MD5 below

	fh, err := os.OpenFile(part, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	defer fh.Close()
	// Bypass the buffer cache where the platform allows (macOS). Otherwise
	// macOS takes writes far faster than a USB stick absorbs them, and
	// closing the file -- also on Ctrl+C -- waits for the backlog, several
	// seconds per GB. Uncached sequential 4 MiB writes are no slower on a
	// stick, and they take as long as the disk really needs, which is what
	// the concurrency control and the status display go by.
	noCache(fh)
	chunkLen := func(i int64) int64 { return min(f.Chunk, f.Size-i*f.Chunk) }

	// Resume: keep chunks of an earlier .part that already verify.
	st, err := fh.Stat()
	if err != nil {
		return err
	}
	valid := make([]bool, n)
	var todo []int64
	buf := make([]byte, f.Chunk)
	d.st.file, d.st.fileSize, d.st.fileDone = f.Path, min(st.Size(), f.Size), 0
	d.st.checking = st.Size() > 0
	for i := int64(0); i < n; i++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		start, l := i*f.Chunk, chunkLen(i)
		if crcs[i] != "" && start+l <= st.Size() {
			if _, err := fh.ReadAt(buf[:l], start); err == nil &&
				fmt.Sprintf("%08X", crc32.ChecksumIEEE(buf[:l])) == strings.ToUpper(crcs[i]) {
				valid[i] = true
			}
			d.st.fileDone += l
			d.progress(false)
		}
		if !valid[i] {
			todo = append(todo, i)
		}
	}
	d.st.checking = false

	// The writer. It keeps draining after an error so the loop below never
	// blocks on it, and reports the error when the queue is closed.
	type write struct {
		off  int64
		data []byte
	}
	queue := make(chan write, window)
	writeErr := make(chan error, 1)
	var abort atomic.Bool // stopping early: drop what's queued (a rerun resumes)
	go func() {
		var err error
		for w := range queue {
			if err != nil || abort.Load() {
				continue
			}
			t := time.Now()
			_, err = fh.WriteAt(w.data, w.off)
			d.writeNanos.Add(int64(time.Since(t)))
			if err == nil {
				d.writeBytes.Add(int64(len(w.data)))
				d.done.Add(int64(len(w.data)))
			}
		}
		writeErr <- err
	}()
	queueOpen := true
	finishWrites := func() error {
		queueOpen = false
		close(queue)
		return <-writeErr
	}
	defer func() { // before fh.Close: never close the file under the writer
		if queueOpen {
			abort.Store(true)
			finishWrites()
		}
	}()

	urlPath := partner + "/" + m.PackageID + "/" + m.VersionID + "/" + f.Path
	pending := map[int64]*chunkJob{}
	next := 0
	launchNext := func() {
		j := todo[next]
		job := &chunkJob{results: make(chan chunkResult, 2), started: time.Now(), size: chunkLen(j)}
		pending[j] = job
		d.launch(ctx, urlPath, f, j, crcs[j], "", job)
		next++
	}
	topUp := func(i int64) {
		for next < len(todo) && todo[next] < i+window && d.inFlight.Load() < int64(d.cc.Limit()) {
			launchNext()
		}
		// The chunk needed next can't wait for a free place: losing hedges
		// may still be running and count towards the limit.
		if next < len(todo) && todo[next] == i {
			launchNext()
		}
	}
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()

	d.st.fileBase, d.st.fileSize = d.done.Load(), f.Size
	// Let the status display show the blocks from the next one needed on.
	var head int64
	d.st.cells = func() []cell {
		cells := make([]cell, window)
		for k := range cells {
			j := head + int64(k)
			job := pending[j]
			switch {
			case j >= n:
				cells[k].kind = cellNone
			case job == nil:
				cells[k].kind = cellQueued
			case len(job.results) > 0:
				cells[k].kind = cellReady
			default:
				got := max(job.got[0].Load(), job.got[1].Load())
				cells[k] = cell{kind: cellLoading, frac: float64(got) / float64(job.size),
					retried: job.retried.Load(), hedged: job.launched > 1}
			}
		}
		return cells
	}
	defer func() { d.st.cells = nil }()

	h := md5.New()
	for i := int64(0); i < n; i++ {
		head = i
		start := i * f.Chunk
		if valid[i] {
			data := buf[:chunkLen(i)]
			if _, err := fh.ReadAt(data, start); err != nil {
				return err
			}
			h.Write(data)
			d.done.Add(int64(len(data)))
		} else {
			// Keep the pipe full while waiting for the next chunk. Everything
			// behind it waits too, so if it straggles (a slow node or
			// connection), ask another node as well and take whichever
			// answer comes first.
			topUp(i)
			job := pending[i]
			var data []byte
			for data == nil {
				if job.launched == 1 && time.Since(job.started) > d.hedgeAfter() {
					avoid := ""
					if h := job.host.Load(); h != nil {
						avoid = *h
					}
					d.launch(ctx, urlPath, f, i, crcs[i], avoid, job)
					d.hedges.Add(1)
				}
				select {
				case r := <-job.results:
					if r.err == nil {
						data = r.data
					} else if job.failed++; job.failed == job.launched {
						return r.err
					}
				case <-d.wake:
				case <-tick.C:
				case <-ctx.Done():
					return ctx.Err()
				}
				topUp(i)
				d.cc.tick(d.net.Load())
				d.progress(false)
			}
			delete(pending, i)
			head = i + 1 // it's out of the buffer, into the write queue
			h.Write(data)
			// Hand it to the writer. This only blocks when the disk is a
			// whole queue behind -- then the disk is the bottleneck.
			t := time.Now()
			for sent := false; !sent; {
				select {
				case queue <- write{start, data}:
					sent = true
				case <-tick.C:
					d.progress(false)
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			blocked := time.Since(t)
			d.diskWait += blocked
			d.cc.blockedOnDisk(blocked)
		}
		head = i + 1
		topUp(i + 1)
		d.cc.tick(d.net.Load())
		d.progress(false)
	}
	if err := finishWrites(); err != nil {
		return err
	}
	if err := fh.Truncate(f.Size); err != nil {
		return err
	}
	t := time.Now()
	err = fh.Sync()
	d.writeNanos.Add(int64(time.Since(t)))
	if err != nil {
		return err
	}
	if err := fh.Close(); err != nil {
		return err
	}
	if !strings.EqualFold(hex.EncodeToString(h.Sum(nil)), f.MD5) {
		os.Remove(part)
		return errors.New(f.Path + ": MD5 mismatch after download; removed, rerun to retry")
	}
	if keepPart {
		return nil
	}
	return os.Rename(part, final)
}

// nodeShares describes how much each node served, e.g. "node10 93%, node9 7%".
func (d *Downloader) nodeShares() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var total int64
	for _, st := range d.stats {
		total += st.bytes
	}
	if total == 0 {
		return ""
	}
	type share struct {
		host  string
		bytes int64
	}
	var shares []share
	for h, st := range d.stats {
		if st.bytes > 0 {
			shares = append(shares, share{strings.SplitN(h, ".", 2)[0], st.bytes})
		}
	}
	slices.SortFunc(shares, func(a, b share) int { return cmp.Compare(b.bytes, a.bytes) })
	var parts []string
	for _, sh := range shares {
		parts = append(parts, fmt.Sprintf("%s %.0f%%", sh.host, 100*float64(sh.bytes)/float64(total)))
	}
	return strings.Join(parts, ", ")
}
