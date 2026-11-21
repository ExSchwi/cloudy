package player

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/egoist/mygo/audio"

	"netease-music-mygo/internal/netease"
)

// A mid-stream connection drop on a Range-capable server resumes transparently
// from the last byte received without losing the partial download or breaking playback.
func TestDownloadResumesMidStreamRange(t *testing.T) {
	b, err := os.ReadFile("testdata/seek.mp3")
	if err != nil {
		t.Fatal(err)
	}
	ref := decodeAll(t, b)
	cut := len(b) / 2
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := requests.Add(1)
		rng := r.Header.Get("Range")
		if n == 1 && rng == "" {
			w.Header().Set("Content-Length", strconv.Itoa(len(b)))
			w.WriteHeader(http.StatusOK)
			w.Write(b[:cut])
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			// Drop the connection before sending the rest of Content-Length.
			if hj, ok := w.(http.Hijacker); ok {
				if conn, _, err := hj.Hijack(); err == nil {
					conn.Close()
				}
			}
			return
		}
		if strings.HasPrefix(rng, "bytes=") {
			fromStr, _, _ := strings.Cut(strings.TrimPrefix(rng, "bytes="), "-")
			from, err := strconv.Atoi(fromStr)
			if err != nil || from < 0 || from >= len(b) {
				w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
				return
			}
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", from, len(b)-1, len(b)))
			w.Header().Set("Content-Length", strconv.Itoa(len(b)-from))
			w.WriteHeader(http.StatusPartialContent)
			w.Write(b[from:])
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write(b)
	}))
	defer srv.Close()

	c := NewCache(t.TempDir(), 1<<30)
	st := &netease.Stream{URL: srv.URL, Size: int64(len(b)), Level: "exhigh", Type: "mp3"}
	dl := c.download(11, st)
	s, err := openStream(dl, int64(len(ref)/2)*1000/44100)
	if err != nil {
		t.Fatal(err)
	}
	var got []float32
	buf := make([]float32, 4096)
	for {
		n, err := s.Read(buf)
		got = append(got, buf[:n]...)
		if err != nil {
			if !errors.Is(err, io.EOF) {
				t.Fatalf("read failed after mid-stream drop: %v", err)
			}
			break
		}
	}
	s.Close()
	if requests.Load() < 2 {
		t.Fatalf("expected Range resume request, got %d requests", requests.Load())
	}
	if len(got) != len(ref) || maxDiff(got, ref) != 0 {
		t.Fatalf("decoded %d of %d samples, diff=%v", len(got), len(ref), maxDiff(got, ref))
	}
	if kept, _, ok := c.lookup(11, "exhigh"); !ok {
		t.Fatal("resumed download was not cached")
	} else {
		kept.Close()
	}
}

// When HTTP Content-Length differs from stale API metadata size, the download
// adopts Content-Length so it finishes and caches cleanly instead of looping on unexpected EOF.
func TestDownloadContentLengthOverridesStaleAPISize(t *testing.T) {
	b, err := os.ReadFile("testdata/seek.mp3")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(b)))
		w.Write(b)
	}))
	defer srv.Close()

	c := NewCache(t.TempDir(), 1<<30)
	// API claims the song is 4096 bytes larger than the real file on CDN.
	st := &netease.Stream{URL: srv.URL, Size: int64(len(b)) + 4096, Level: "exhigh", Type: "mp3"}
	dl := c.download(12, st)
	dl.waitFor(int64(len(b)))
	// Wait for fetch to finish hand-off.
	dl.mu.Lock()
	for !dl.done {
		dl.cond.Wait()
	}
	dlErr := dl.err
	dlSize := dl.size
	dl.mu.Unlock()
	dl.Close()
	if dlErr != nil || dlSize != int64(len(b)) {
		t.Fatalf("stale API size broke completion: err=%v size=%d want=%d", dlErr, dlSize, len(b))
	}
	if kept, _, ok := c.lookup(12, "exhigh"); !ok {
		t.Fatal("download with shorter Content-Length was not cached")
	} else {
		kept.Close()
	}
}

// Promoting an ahead fetcher to head must never advance d.n past the bytes
// actually written to the file.
func TestDownloadHeadAheadMergeDoesNotSkipUnwrittenBytes(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "merge-*.part")
	if err != nil {
		t.Fatal(err)
	}
	dl := &download{file: f, part: f.Name(), size: 1000, users: 1}
	dl.cond.L = &dl.mu
	defer dl.release()

	head := &fetcher{head: true}
	ahead := &fetcher{}
	dl.ahead, dl.a0, dl.a1 = ahead, 100, 120
	// Head writes bytes [0..150), surpassing both a0 (100) and a1 (120),
	// and promotes ahead to head with d.n = 150.
	state, err := dl.put(head, bytes.Repeat([]byte{1}, 150), 0)
	if err != nil || state != putStop || !ahead.head {
		t.Fatalf("merge state=%d err=%v ahead.head=%v", state, err, ahead.head)
	}
	// Ahead had written [120..140) before learning it became head.
	// d.n must stay 150, NOT become 150 + 20 = 170 (which would expose unwritten zeros).
	if _, err := dl.put(ahead, bytes.Repeat([]byte{2}, 20), 120); err != nil {
		t.Fatal(err)
	}
	if dl.n != 150 {
		t.Fatalf("d.n = %d after promoted ahead overlap, want 150", dl.n)
	}
}

// A FLAC with a large PICTURE metadata block and a non-standard comment block
// opens via Range jump and STREAMINFO-only parsing without downloading the cover first.
func TestFLACLargePictureBlockOpensViaRange(t *testing.T) {
	b, err := os.ReadFile("testdata/seek.flac")
	if err != nil {
		t.Fatal(err)
	}
	ref := decodeAll(t, b)
	origStart, err := flacDataStart(downloaded(b))
	if err != nil {
		t.Fatal(err)
	}
	// Construct: "fLaC" + STREAMINFO (not last) + 512KB PICTURE (not last) +
	// malformed VORBIS_COMMENT (last) + original FLAC audio frames.
	const streamInfoEnd = 4 + 4 + 34
	var full []byte
	full = append(full, b[:streamInfoEnd]...)
	full[4] &^= 0x80 // STREAMINFO is not the last metadata block

	picLen := 512 << 10
	full = append(full, 6, byte(picLen>>16), byte(picLen>>8), byte(picLen))
	full = append(full, make([]byte, picLen)...)

	// Malformed VORBIS_COMMENT payload (type 4, last=1) that full flac.New rejects.
	badComment := []byte{0xff, 0xff, 0xff, 0xff}
	full = append(full, 0x80|4, 0, 0, byte(len(badComment)))
	full = append(full, badComment...)
	full = append(full, b[origStart:]...)

	headBlocked := make(chan struct{})
	defer close(headBlocked)
	var rangeHits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rng := r.Header.Get("Range")
		if strings.HasPrefix(rng, "bytes=") {
			rangeHits.Add(1)
			fromStr, _, _ := strings.Cut(strings.TrimPrefix(rng, "bytes="), "-")
			from, _ := strconv.Atoi(fromStr)
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", from, len(full)-1, len(full)))
			w.Header().Set("Content-Length", strconv.Itoa(len(full)-from))
			w.WriteHeader(http.StatusPartialContent)
			w.Write(full[from:])
			return
		}
		// Head sends only the first 4KB (containing STREAMINFO + PICTURE header) then stalls.
		w.Header().Set("Content-Length", strconv.Itoa(len(full)))
		w.WriteHeader(http.StatusOK)
		w.Write(full[:4096])
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		select {
		case <-headBlocked:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()

	c := NewCache(t.TempDir(), 1<<30)
	dl := c.download(13, &netease.Stream{URL: srv.URL, Size: int64(len(full)), Level: "lossless", Type: "flac"})
	s, err := openStream(dl, 5000)
	if err != nil {
		dl.Close()
		t.Fatalf("FLAC with huge picture + malformed comment failed to open: %v", err)
	}
	defer s.Close()
	buf := make([]float32, 4096)
	n, err := s.Read(buf)
	if err != nil || n == 0 {
		t.Fatalf("first FLAC read failed: n=%d err=%v", n, err)
	}
	if maxDiff(buf[:n], ref[:n]) != 0 {
		t.Fatal("FLAC decoded samples did not match reference")
	}
	if rangeHits.Load() == 0 {
		t.Fatal("expected Range jump over large PICTURE block")
	}
}

// Consecutive ID3v2 tags and an ID3v2.4 footer before an MP3 stream open and
// decode identically to the untagged audio.
func TestMP3ConsecutiveID3AndFooterOpenCleanly(t *testing.T) {
	b, err := os.ReadFile("testdata/seek.mp3")
	if err != nil {
		t.Fatal(err)
	}
	ref := decodeAll(t, b)
	// First tag: ID3v2.4 with footer flag (0x10), body 16 bytes + 10-byte footer.
	tag1 := append([]byte{'I', 'D', '3', 4, 0, 0x10, 0, 0, 0, 16}, make([]byte, 16+10)...)
	// Second tag: ID3v2.3, body 24 bytes.
	tag2 := append([]byte{'I', 'D', '3', 3, 0, 0, 0, 0, 0, 24}, make([]byte, 24)...)
	tagged := append(append(tag1, tag2...), b...)

	s, err := openStream(downloaded(tagged), int64(len(ref)/2)*1000/44100)
	if err != nil {
		t.Fatalf("double-tagged MP3 failed to open: %v", err)
	}
	defer s.Close()
	buf := make([]float32, 4096)
	n, err := s.Read(buf)
	if err != nil || n == 0 || maxDiff(buf[:n], ref[:n]) > 1e-4 {
		t.Fatalf("double-tagged MP3 decode mismatch: n=%d err=%v diff=%v", n, err, maxDiff(buf[:n], ref[:n]))
	}
}

// unknownLenSilence is a decoder with unknown length (Len == -1).
type unknownLenSilence struct{}

func (unknownLenSilence) SampleRate() int               { return 48000 }
func (unknownLenSilence) Channels() int                 { return 2 }
func (unknownLenSilence) Len() int64                    { return -1 }
func (unknownLenSilence) SetPosition(int64) error       { return nil }
func (unknownLenSilence) Read(b []float32) (int, error) { clear(b); return len(b), nil }

// Resuming an unknown-duration stream after pausing must not rewind to 0:00.
func TestTogglePlayUnknownDurationDoesNotRewind(t *testing.T) {
	ch := make(chan func(), 16)
	p := New(func(fn func()) { ch <- fn })
	p.Cue([]netease.Track{{ID: 1, Name: "live"}})
	out := audio.NewPlayer(unknownLenSilence{})
	defer out.Close()
	out.SetVolume(0)
	out.Seek(45 * time.Second)
	p.Attach(out)
	p.fade.attach(out, 1)
	p.playing = false
	p.TogglePlay()
	if pos := p.Position(); pos < 40*time.Second {
		t.Fatalf("resuming unknown-duration stream rewound to %v", pos)
	}
}

type failingSeekStream struct{ stream }

func (failingSeekStream) SetPosition(int64) error { return errors.New("seek failed") }

// Seek errors on checkedStream must be preserved in Err() so finish() retries
// instead of treating the failure as a natural track end.
func TestCheckedStreamRecordsSeekError(t *testing.T) {
	s := &checkedStream{stream: failingSeekStream{&mp3Stream{dl: downloaded(nil)}}}
	if err := s.SetPosition(100); err == nil {
		t.Fatal("expected seek error")
	}
	if s.Err() == nil || s.Err().Error() != "seek failed" {
		t.Fatalf("checkedStream.Err() = %v, want seek failed", s.Err())
	}
}

// A completely downloaded track whose actual audio is a few seconds shorter
// than NetEase's metadata duration advances naturally instead of retrying forever.
func TestFinishWholeStreamToleratesShortMetadataDrift(t *testing.T) {
	quietNavigation(t)
	b, err := os.ReadFile("testdata/seek.mp3")
	if err != nil {
		t.Fatal(err)
	}
	p, ch := navigationPlayer(t, []netease.Track{{ID: 1, Duration: 600000}, {ID: 2, Duration: 5000}})
	cacheNavigationTrack(t, p, 2)
	dl := downloaded(b)
	s := &checkedStream{stream: &mp3Stream{dl: dl, drop: dl.Close, rate: 48000, frames: 48000 * 600}}
	p.stream, p.out, p.playing = s, audio.NewPlayer(silence{}), true
	// Played to 595s of 600s declared duration (5s metadata drift) on a whole download.
	p.out.Seek(595 * time.Second)
	p.finish(s)
	finishNavigation(t, p, ch)
	if p.Current().ID != 2 || !p.Playing() || p.Err() != "" {
		t.Fatalf("whole stream with 5s metadata drift did not advance: id=%d error=%q", p.Current().ID, p.Err())
	}
}

// takePreload discards a preloaded stream whose background download has
// already failed instead of handing a broken stream to opened().
func TestTakePreloadDiscardsFailedDownload(t *testing.T) {
	p := &Player{at: 0, queue: []netease.Track{{ID: 1}, {ID: 2}}, order: []int{0, 1}}
	dl := downloaded(nil)
	dl.err = io.ErrUnexpectedEOF
	ch := make(chan struct{})
	close(ch)
	p.pre = &preload{
		at:     1,
		id:     2,
		level:  "exhigh",
		cancel: func() {},
		audio:  ch,
		s:      &mp3Stream{dl: dl, drop: dl.Close},
	}
	if got := p.takePreload(1, "exhigh", 0, 0); got != nil {
		t.Fatal("takePreload returned a preload whose download had already failed")
	}
}

// A starved playback downloadReader accumulates a cushion of bytes before
// returning so slow bursty downloads do not stutter frame by frame.
func TestDownloadRebufferWaitsAfterStarvation(t *testing.T) {
	dl := &download{size: 128 << 10, cancel: func() {}, users: 1}
	dl.cond.L = &dl.mu
	r := &downloadReader{d: dl, rebuffer: true}
	gotCh := make(chan int, 1)
	go func() {
		buf := make([]byte, 64<<10)
		n, _ := r.Read(buf)
		gotCh <- n
	}()
	// Let the reader starve on 0 bytes, then deliver 8KB followed by 64KB.
	time.Sleep(20 * time.Millisecond)
	_ = dl.write(make([]byte, 8<<10))
	select {
	case n := <-gotCh:
		t.Fatalf("rebuffer returned early with only %d bytes after starvation", n)
	case <-time.After(40 * time.Millisecond):
	}
	_ = dl.write(make([]byte, 64<<10))
	select {
	case n := <-gotCh:
		if n < 64<<10 {
			t.Fatalf("rebuffer returned %d bytes, want >= 64KB", n)
		}
	case <-time.After(time.Second):
		t.Fatal("rebuffer did not wake after cushion filled")
	}
	dl.Close()
}

// When requesting "hires" resolves to a lower tier ("lossless" or "exhigh"),
// the cache records the downgraded mapping so replays hit disk with zero
// network calls, and ResetQualityHints clears those aliases on account switch.
func TestCacheDowngradedQualityHitsOnReplay(t *testing.T) {
	b, err := os.ReadFile("testdata/seek.flac")
	if err != nil {
		t.Fatal(err)
	}
	var audioReqs atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		audioReqs.Add(1)
		w.Header().Set("Content-Length", strconv.Itoa(len(b)))
		w.Write(b)
	}))
	defer srv.Close()

	c := NewCache(t.TempDir(), 1<<30)
	dl := c.downloadFor(901, "hires", &netease.Stream{URL: srv.URL, Size: int64(len(b)), Level: "lossless", Type: "flac"})
	// Wait for download + cache hand-off to complete.
	dl.mu.Lock()
	for !dl.done {
		dl.cond.Wait()
	}
	dl.mu.Unlock()
	dl.Close()

	// Replay at "hires" must hit the cached lossless copy directly.
	hit, st, ok := c.lookup(901, "hires")
	if !ok || st.Level != "lossless" || hit.size != int64(len(b)) {
		t.Fatalf("downgraded hires replay missed cache: ok=%v st=%+v", ok, st)
	}
	hit.Close()
	if audioReqs.Load() != 1 {
		t.Fatalf("expected 1 audio download, got %d", audioReqs.Load())
	}

	// Clearing quality hints makes "hires" re-check while "lossless" still hits.
	c.ResetQualityHints()
	if _, _, ok := c.lookup(901, "hires"); ok {
		t.Fatal("hires still hit after ResetQualityHints")
	}
	if hit, _, ok := c.lookup(901, "lossless"); !ok {
		t.Fatal("lossless missed after ResetQualityHints")
	} else {
		hit.Close()
	}
}

// An existing pre-hint cached file (<id>-lossless.flac) is rescued immediately
// when SongURL returns Level="lossless" for a "hires" request, downloading 0
// audio bytes and making all subsequent "hires" plays 0-network cache hits.
func TestOpenContextRescuesExistingCacheAfterDowngrade(t *testing.T) {
	b, err := os.ReadFile("testdata/seek.flac")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	c := NewCache(dir, 1<<30)
	// Seed a legacy cached lossless file without maxRequested metadata.
	f := c.part(&netease.Stream{Size: int64(len(b)), Level: "lossless", Type: "flac"})
	f.Write(b)
	f.Close()
	path, ok := c.keep(902, &netease.Stream{Size: int64(len(b)), Level: "lossless", Type: "flac"}, f.Name(), int64(len(b)))
	if !ok {
		t.Fatal("failed to seed legacy cache file")
	}
	c.settled(path)

	var audioHits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		audioHits.Add(1)
		w.Write(b)
	}))
	defer srv.Close()

	var urlCalls atomic.Int32
	oldSongURL := songURL
	songURL = func(ctx context.Context, id int64, level string) (*netease.Stream, error) {
		urlCalls.Add(1)
		return &netease.Stream{URL: srv.URL, Size: int64(len(b)), Level: "lossless", Type: "flac"}, nil
	}
	t.Cleanup(func() { songURL = oldSongURL })

	p := &Player{Cache: c}
	tr := netease.Track{ID: 902, Duration: 3000}

	// Play 1: songURL resolves hires -> lossless, rescued from disk with 0 HTTP audio requests.
	st, s, err := p.openContext(context.Background(), tr, "hires")
	if err != nil || st.Level != "lossless" {
		t.Fatalf("play 1 failed: st=%+v err=%v", st, err)
	}
	s.Close()
	if urlCalls.Load() != 1 || audioHits.Load() != 0 {
		t.Fatalf("play 1 should rescue disk cache: urlCalls=%d audioHits=%d", urlCalls.Load(), audioHits.Load())
	}

	// Play 2: alias was persisted, so neither songURL nor HTTP audio is called.
	st, s, err = p.openContext(context.Background(), tr, "hires")
	if err != nil || st.Level != "lossless" {
		t.Fatalf("play 2 failed: st=%+v err=%v", st, err)
	}
	s.Close()
	if urlCalls.Load() != 1 || audioHits.Load() != 0 {
		t.Fatalf("play 2 should hit disk directly: urlCalls=%d audioHits=%d", urlCalls.Load(), audioHits.Load())
	}
}

// On Windows, renaming a .part over an open destination file fails; keepFor
// reuses an identical-size destination file and still writes updated metadata.
func TestCacheKeepReusesOpenDestination(t *testing.T) {
	dir := t.TempDir()
	c := NewCache(dir, 1<<20)
	dest := filepath.Join(dir, "903-lossless.flac")
	payload := bytes.Repeat([]byte("f"), 1024)
	if err := os.WriteFile(dest, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	// Hold destination open (blocks os.Rename overwrite on Windows).
	held, err := os.Open(dest)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()

	part := filepath.Join(dir, "incoming.part")
	if err := os.WriteFile(part, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	gotPath, ok := c.keepFor(903, "hires", &netease.Stream{Level: "lossless", Type: "flac"}, part, int64(len(payload)))
	if !ok || gotPath != dest {
		t.Fatalf("keepFor failed with open destination: ok=%v path=%q", ok, gotPath)
	}
	c.settled(gotPath)
	if hit, _, ok := c.lookup(903, "hires"); !ok {
		t.Fatal("hires alias not saved when destination was held open")
	} else {
		hit.Close()
	}
}

// prebuffer accumulates an initial cushion on active downloads and returns
// immediately on completed downloads.
func TestDownloadPrebufferWaitsForInitialCushion(t *testing.T) {
	// Completed download returns immediately.
	doneDL := downloaded(make([]byte, 1024))
	start := time.Now()
	doneDL.prebuffer(0, 256<<10, 500*time.Millisecond)
	if time.Since(start) > 50*time.Millisecond {
		t.Fatal("prebuffer waited on completed download")
	}

	// Active download waits until cushion arrives.
	dl := &download{size: 256 << 10, cancel: func() {}, users: 1}
	dl.cond.L = &dl.mu
	done := make(chan struct{})
	go func() {
		dl.prebuffer(0, 128<<10, time.Second)
		close(done)
	}()
	_ = dl.write(make([]byte, 32<<10))
	select {
	case <-done:
		t.Fatal("prebuffer returned before cushion filled")
	case <-time.After(30 * time.Millisecond):
	}
	_ = dl.write(make([]byte, 128<<10))
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("prebuffer did not wake after cushion filled")
	}
	dl.Close()
}
