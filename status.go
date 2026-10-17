package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"golang.org/x/term"
)

// The live status shown while downloading: a block of lines redrawn in
// place on a terminal, or a line every 10 s when output goes elsewhere.

type rateSample struct {
	t         time.Time
	net       int64
	written   int64
	writeTime time.Duration
	diskWait  time.Duration
}

type cellKind int

const (
	cellNone    cellKind = iota // past the end of the file
	cellQueued                  // not requested yet
	cellLoading                 // being downloaded
	cellReady                   // downloaded, waiting to be written
)

// cell is one block in the buffer ahead of the writer.
type cell struct {
	kind            cellKind
	frac            float64 // of a loading block, received so far
	retried, hedged bool
}

type status struct {
	tty   bool
	drawn int // lines of the block currently on screen
	shown time.Time

	region, file       string
	fileIdx, fileCount int
	fileDone, fileSize int64 // fileDone: while checking only
	fileBase           int64 // Downloader.done when the file's download began
	samples            []rateSample
	cells              func() []cell // the buffer, while a file downloads
	checking           bool          // reading back an earlier .part file
	color              bool
}

func newStatus() status {
	tty := term.IsTerminal(int(os.Stdout.Fd()))
	if tty {
		setupTerminal()
	}
	return status{tty: tty, color: tty && os.Getenv("NO_COLOR") == ""}
}

func termWidth() int {
	w, _, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || w <= 0 {
		return 80
	}
	return w
}

func fmtDuration(d time.Duration) string {
	d = d.Round(time.Second)
	switch {
	case d >= time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	case d >= time.Minute:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%ds", int(d.Seconds()))
}

func (s *status) bar(frac float64, width int) string {
	n := int(frac*float64(width) + 0.5)
	n = min(max(n, 0), width)
	return s.paint(ansiGreen, strings.Repeat("█", n)) + s.paint(ansiDim, strings.Repeat("░", width-n))
}

// rateSince is the download rate over (roughly) the last window, from the
// samples taken at each redraw; ok is false until there's enough history.
func (s *status) rateSince(window time.Duration) (rate float64, ok bool) {
	first, last, ok := s.span(window)
	dt := last.t.Sub(first.t).Seconds()
	if !ok || dt < window.Seconds()/2 {
		return 0, false
	}
	return float64(last.net-first.net) / dt, true
}

// span returns the oldest sample within window of the latest one, or the
// one before the latest if they're further apart (when not on a terminal,
// samples are 10 s apart).
func (s *status) span(window time.Duration) (first, last rateSample, ok bool) {
	if len(s.samples) < 2 {
		return first, last, false
	}
	last = s.samples[len(s.samples)-1]
	k := len(s.samples) - 2
	for k > 0 && last.t.Sub(s.samples[k-1].t) <= window {
		k--
	}
	return s.samples[k], last, true
}

const (
	ansiReset   = "\x1b[0m"
	ansiBold    = "\x1b[1m"
	ansiDim     = "\x1b[2m"
	ansiRed     = "\x1b[31m"
	ansiGreen   = "\x1b[32m"
	ansiYellow  = "\x1b[33m"
	ansiMagenta = "\x1b[35m"
	ansiBlue    = "\x1b[34m"
	ansiCyan    = "\x1b[36m"
)

func (s *status) paint(color, text string) string {
	return paintIf(s.color, color, text)
}

// useColor says whether to colour output: on a terminal, unless NO_COLOR
// is set.
func useColor() bool {
	return term.IsTerminal(int(os.Stdout.Fd())) && os.Getenv("NO_COLOR") == ""
}

func paintIf(on bool, color, text string) string {
	if !on || text == "" || color == "" {
		return text
	}
	return color + text + ansiReset
}

// blocks draws the buffer: one cell per block from the one the writer
// waits for (leftmost) on. A loading block fills up as data arrives.
func (s *status) blocks(cells []cell) string {
	if cells == nil {
		return ""
	}
	levels := []rune("▁▂▃▄▅▆▇")
	var b strings.Builder
	for _, c := range cells {
		switch c.kind {
		case cellNone:
			b.WriteString(" ")
		case cellQueued:
			b.WriteString(s.paint(ansiDim, "·"))
		case cellReady:
			b.WriteString(s.paint(ansiGreen, "█"))
		case cellLoading:
			ch := string(levels[min(int(c.frac*float64(len(levels))), len(levels)-1)])
			color := ansiYellow
			if c.hedged {
				color = ansiMagenta
			} else if c.retried {
				color = ansiRed
			}
			b.WriteString(s.paint(color, ch))
		}
	}
	legend := s.paint(ansiGreen, "█") + " ready " + s.paint(ansiYellow, "▄") + " loading " +
		s.paint(ansiRed, "▄") + " retry " + s.paint(ansiMagenta, "▄") + " hedged " + s.paint(ansiDim, "·") + " queued"
	if !s.color {
		legend = "█ ready  ▄ loading  · queued"
	}
	return "  buffer ▕" + b.String() + "▏  " + legend
}

// truncate cuts l to width visible characters, not counting colour codes.
func truncate(l string, width int) string {
	var b strings.Builder
	visible, esc := 0, false
	for _, r := range l {
		switch {
		case esc:
			b.WriteRune(r)
			esc = r != 'm'
		case r == '\x1b':
			b.WriteRune(r)
			esc = true
		case visible < width:
			b.WriteRune(r)
			visible++
		}
	}
	return b.String()
}

// stickStatus describes the destination volume and how hard it's working:
// its write speed while writing and the share of time spent writing, over
// the last 5 s. On macOS writes bypass the buffer cache, so that's the
// device's real speed; elsewhere it partly reflects the cache.
//
// The label takes labelColor (the bottleneck's colour, or none), and busy
// goes from green to red as the stick saturates.
func (d *Downloader) stickStatus(labelColor string) (long, short string) {
	freeB, totalB, _, err := diskSpace(d.dest)
	if err != nil {
		return "stick: " + err.Error(), ""
	}
	name, fs := volumeInfo(d.dest)
	free, capacity := float64(freeB), float64(totalB)

	speed, busy, busyShown := "-", "-", "-"
	if first, last, ok := d.st.span(5 * time.Second); ok {
		if dt := last.t.Sub(first.t); dt > time.Second {
			wt := last.writeTime - first.writeTime
			frac := min(wt.Seconds()/dt.Seconds(), 1)
			busy = fmt.Sprintf("%.0f%%", 100*frac)
			busyColor := ansiGreen
			switch {
			case frac > 0.9:
				busyColor = ansiRed
			case frac > 0.5:
				busyColor = ansiYellow
			}
			busyShown = d.st.paint(busyColor, busy)
			if wt > 0 {
				speed = human(float64(last.written-first.written)/wt.Seconds()) + "/s"
			}
		}
	}
	long = fmt.Sprintf("%s %s (%s) · %s free of %s · writing %s · busy %s",
		d.st.paint(labelColor, "stick:"), name, fs, human(free), human(capacity), speed, busyShown)
	short = fmt.Sprintf("stick %s free, busy %s", human(free), busy)
	return long, short
}

// limitedBy says whether the network or the stick held things up over the
// last few seconds: the stick, if the loop handing out requests often had
// to wait for the writer to take a block.
func (s *status) limitedBy() string {
	if s.checking {
		return "checking"
	}
	first, last, ok := s.span(3 * time.Second)
	dt := last.t.Sub(first.t).Seconds()
	if !ok || dt < 1 || last.net == first.net {
		return "measuring"
	}
	if (last.diskWait-first.diskWait).Seconds()/dt > ccDiskBound {
		return "stick"
	}
	return "network"
}

// clear removes the status block from the screen, so other output can be
// printed in its place.
func (s *status) clear() {
	if s.tty && s.drawn > 0 {
		fmt.Printf("\x1b[%dF\x1b[J", s.drawn)
		s.drawn = 0
	}
}

// printf prints a message above the status block.
func (d *Downloader) printf(format string, a ...any) {
	d.st.clear()
	fmt.Printf(format, a...)
	d.progress(true)
}

// finish draws the status one last time and leaves it on screen.
func (d *Downloader) finish() {
	d.progress(true)
	if !d.st.tty {
		return
	}
	d.st.drawn = 0
}

func (d *Downloader) progress(force bool) {
	s := &d.st
	now := time.Now()
	every := 500 * time.Millisecond
	if !s.tty {
		every = 10 * time.Second
	}
	if !force && now.Sub(s.shown) < every {
		return
	}
	s.shown = now

	net := d.net.Load()
	s.samples = append(s.samples, rateSample{now, net, d.writeBytes.Load(), time.Duration(d.writeNanos.Load()), d.diskWait})
	for len(s.samples) > 2 && now.Sub(s.samples[0].t) > 20*time.Second {
		s.samples = s.samples[1:]
	}
	avg := float64(net) / now.Sub(d.t0).Seconds()
	cur, ok := s.rateSince(5 * time.Second)
	if !ok {
		cur = avg
	}
	etaRate, ok := s.rateSince(15 * time.Second)
	if !ok {
		etaRate = avg
	}
	eta := "-"
	done := d.done.Load()
	if left := d.total - done; left <= 0 {
		eta = "0s"
	} else if etaRate > 0 {
		eta = fmtDuration(time.Duration(float64(left) / etaRate * float64(time.Second)))
	}

	limit := d.cc.Limit()
	limitedBy := s.limitedBy()
	// Every server in ranking order, the one most requests go to first.
	var servers, compact []string
	var inFlight int64
	d.mu.Lock()
	for k, n := range d.nodes {
		st := d.stats[n.Host]
		name := strings.SplitN(n.Host, ".", 2)[0]
		mark := ""
		if k == 0 {
			mark = s.paint(ansiBold, "▶")
		}
		desc := fmt.Sprintf("%s%s %d ms: ", mark, name, st.rtt.Milliseconds())
		a := st.active.Load()
		inFlight += a
		switch {
		case a > 0:
			desc += s.paint(ansiGreen, fmt.Sprintf("%d active", a)) + ", " + human(float64(st.bytes))
			compact = append(compact, fmt.Sprintf("%s x%d", name, a))
		case st.bytes > 0:
			desc = s.paint(ansiDim, desc+"idle, "+human(float64(st.bytes)))
		default:
			desc = s.paint(ansiDim, desc+"idle")
		}
		servers = append(servers, desc)
	}
	d.mu.Unlock()
	pct := 100 * float64(done) / float64(max(d.total, 1))
	var cells []cell
	if s.cells != nil {
		cells = s.cells()
	}
	buffered := 0
	for _, c := range cells {
		if c.kind == cellReady {
			buffered++
		}
	}
	fileDone := done - s.fileBase
	if s.checking {
		fileDone = s.fileDone
	}
	filePct := float64(fileDone) / float64(max(s.fileSize, 1))
	// The bottleneck gets a colour, used for its name and its line's label,
	// so a switch between network and stick stands out.
	limitColor := map[string]string{"network": ansiBlue, "stick": ansiCyan}[limitedBy]
	stickLabel := ""
	if limitedBy == "stick" {
		stickLabel = limitColor
	}
	stick, stickShort := d.stickStatus(stickLabel)
	checking := ""
	if s.checking {
		checking = " · checking what's already there"
	}

	if !s.tty {
		active := strings.Join(compact, ", ")
		if active == "" {
			active = "no requests"
		}
		fmt.Printf("%s / %s (%.0f%%) · now %s/s · ETA %s · %s · %s · %s %s\n",
			human(float64(done)), human(float64(d.total)), pct, human(cur), eta,
			active, stickShort, s.region, s.file)
		return
	}
	count := func(color string, n int64) string {
		if n == 0 {
			return "0"
		}
		return s.paint(color, fmt.Sprint(n))
	}
	limitShown := s.paint(limitColor, limitedBy)
	if limitColor == "" {
		limitShown = s.paint(ansiDim, limitedBy)
	}
	serversLabel := "servers:"
	if limitedBy == "network" {
		serversLabel = s.paint(limitColor, serversLabel)
	}
	lines := []string{
		fmt.Sprintf("  %s%s  [%s] %3.0f%%",
			s.paint(ansiBold, fmt.Sprintf("%s · %s (%d/%d)", s.region, s.file, s.fileIdx, s.fileCount)),
			checking, s.bar(filePct, 20), 100*filePct),
		fmt.Sprintf("  %s / %s (%.0f%%) · now %s/s · avg %s/s · ETA %s",
			human(float64(done)), human(float64(d.total)), pct, human(cur), human(avg), s.paint(ansiBold, eta)),
		fmt.Sprintf("  %d in flight (limit %d), %d buffered · limit: %s · retries %s · hedges %s",
			inFlight, limit, buffered, limitShown, count(ansiYellow, d.retries.Load()), count(ansiMagenta, d.hedges.Load())),
		"  " + serversLabel + " " + strings.Join(servers, " · "),
		"  " + stick,
	}

	if l := s.blocks(cells); l != "" {
		lines = append(lines, l)
	}
	width := termWidth()
	var b strings.Builder
	if s.drawn > 0 {
		fmt.Fprintf(&b, "\x1b[%dF", s.drawn)
	}
	for _, l := range lines {
		b.WriteString("\x1b[2K" + truncate(l, width-1) + "\n")
	}
	// The buffer line goes away between files; clear what it left behind.
	b.WriteString("\x1b[J")
	fmt.Print(b.String())
	s.drawn = len(lines)
}
