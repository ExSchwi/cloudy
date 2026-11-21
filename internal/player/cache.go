package player

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"netease-music-mygo/internal/netease"
)

// Cache keeps the songs played on disk, so that they play again at once,
// and without the network: each as <id>-<level>.mp3 or .flac, with what
// its stream was beside it in <id>-<level>.json. Once they take more than
// the limit, those played longest ago go first.
type Cache struct {
	mu      sync.Mutex
	cond    *sync.Cond
	dir     string // where the songs are, which the listener may move
	limit   int64  // in bytes; 0 keeps nothing
	fresh   map[string]int
	dirty   bool
	waiting int
	running bool
}

// pruneHook, when set, runs at the start of a prune. Tests hold it to
// show that keeping a song does not wait for the directory to be cleaned.
var pruneHook atomic.Pointer[func()]

func setPruneHook(fn func()) {
	if fn == nil {
		pruneHook.Store(nil)
		return
	}
	pruneHook.Store(&fn)
}

// NewCache returns a cache in dir of limit bytes.
func NewCache(dir string, limit int64) *Cache {
	c := &Cache{dir: dir, limit: limit, fresh: map[string]int{}}
	c.cond = sync.NewCond(&c.mu)
	return c
}

// SetLimit changes how much the cache may take, and drops what is over.
func (c *Cache) SetLimit(limit int64) {
	c.mu.Lock()
	c.limit = limit
	c.mu.Unlock()
	c.pruneWait()
}

// Dir returns the folder the songs are kept in.
func (c *Cache) Dir() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dir
}

// SetDir keeps the songs in dir from now, and drops those kept where they
// were: the listener moved the cache, as off a disk short of room. A song
// coming in still goes where it began, and is not kept.
func (c *Cache) SetDir(dir string) {
	c.mu.Lock()
	old := c.dir
	c.dir = dir
	c.mu.Unlock()
	if old == dir {
		return
	}
	for _, f := range songsIn(old) {
		c.remove(f.path)
	}
	dropParts(old, time.Hour)
	os.Remove(old) // only once it is empty
	c.pruneWait()
}

func cacheKey(id int64, level string) string { return fmt.Sprintf("%d-%s", id, level) }

// cacheMeta is the JSON metadata stored beside each cached song. It embeds
// netease.Stream for backward compatibility and records the highest quality
// level that resolved to this file so downgraded tracks (e.g. a "hires"
// request that NetEase serves as "lossless" or "exhigh") hit the cache on
// subsequent plays instead of re-downloading every time.
type cacheMeta struct {
	netease.Stream
	MaxRequested string `json:"maxRequested,omitempty"`
	CheckedAt    int64  `json:"checkedAt,omitempty"`
}

// qualityHintTTL bounds how long a downgraded quality mapping is trusted
// before re-checking NetEase (e.g. after VIP changes or catalog upgrades).
const qualityHintTTL = 7 * 24 * time.Hour

func levelIndex(level string) int {
	return slices.Index(netease.Levels, level)
}

func higherLevel(a, b string) string {
	if levelIndex(b) > levelIndex(a) {
		return b
	}
	return a
}

// keepable reports whether a song of level and type ext may be kept: they
// name its file, so only those known, which keep it in the cache's folder.
func keepable(level, ext string) bool {
	return slices.Contains(netease.Levels, level) && (ext == "mp3" || ext == "flac")
}

func (c *Cache) openCached(dir string, id int64, lv string) (*download, *cacheMeta, bool) {
	key := cacheKey(id, lv)
	raw, err := os.ReadFile(filepath.Join(dir, key+".json"))
	if err != nil {
		return nil, nil, false
	}
	var meta cacheMeta
	if json.Unmarshal(raw, &meta) != nil || !keepable(lv, meta.Type) {
		return nil, nil, false
	}
	path := filepath.Join(dir, key+"."+meta.Type)
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, false
	}
	if info, err := f.Stat(); err != nil || info.Size() != meta.Size {
		f.Close()
		return nil, nil, false
	}
	// Played now: the last to go.
	now := time.Now()
	os.Chtimes(path, now, now)
	return opened(f, meta.Size), &meta, true
}

// lookup returns a song kept at level or a better one, or a lower-level copy
// that a recent request for level (or higher) already resolved to.
func (c *Cache) lookup(id int64, level string) (*download, *netease.Stream, bool) {
	if c == nil {
		return nil, nil, false
	}
	dir := c.Dir()
	at := max(slices.Index(netease.Levels, level), 0)
	for _, lv := range netease.Levels[at:] {
		if dl, meta, ok := c.openCached(dir, id, lv); ok {
			return dl, &meta.Stream, true
		}
	}
	for i := at - 1; i >= 0; i-- {
		lv := netease.Levels[i]
		key := cacheKey(id, lv)
		raw, err := os.ReadFile(filepath.Join(dir, key+".json"))
		if err != nil {
			continue
		}
		var meta cacheMeta
		if json.Unmarshal(raw, &meta) != nil || !keepable(lv, meta.Type) {
			continue
		}
		if levelIndex(meta.MaxRequested) < at || meta.CheckedAt <= 0 || time.Since(time.Unix(meta.CheckedAt, 0)) > qualityHintTTL {
			continue
		}
		if dl, openedMeta, ok := c.openCached(dir, id, lv); ok {
			return dl, &openedMeta.Stream, true
		}
	}
	return nil, nil, false
}

// lookupExactAndAlias checks whether actualLevel for id is already cached on
// disk (used right after SongURL resolves a downgraded quality) and, if so,
// records that requesting reqLevel resolved to actualLevel so future lookups
// hit the cache directly without hitting the network.
func (c *Cache) lookupExactAndAlias(id int64, actualLevel, reqLevel string) (*download, *netease.Stream, bool) {
	if c == nil || !slices.Contains(netease.Levels, actualLevel) {
		return nil, nil, false
	}
	dir := c.Dir()
	dl, meta, ok := c.openCached(dir, id, actualLevel)
	if !ok {
		return nil, nil, false
	}
	if levelIndex(reqLevel) > levelIndex(actualLevel) {
		meta.MaxRequested = higherLevel(meta.MaxRequested, reqLevel)
		meta.CheckedAt = time.Now().Unix()
		if raw, err := json.Marshal(meta); err == nil {
			os.WriteFile(filepath.Join(dir, cacheKey(id, actualLevel)+".json"), raw, 0o600)
		}
	}
	return dl, &meta.Stream, true
}

// download fetches the song into a file. Songs the cache does not keep use
// a temporary spool, removed when playback and its readers have finished.
func (c *Cache) download(id int64, st *netease.Stream) *download {
	return c.downloadFor(id, "", st)
}

// downloadFor fetches the song into a file and records reqLevel if NetEase
// served a lower quality than requested.
func (c *Cache) downloadFor(id int64, reqLevel string, st *netease.Stream) *download {
	part := c.part(st)
	if part == nil {
		part, _ = os.CreateTemp("", streamSpools)
		return startDownload(st.URL, st.Size, part, nil, nil)
	}
	keep, settle := c.keeperFor(id, reqLevel, st)
	return startDownload(st.URL, st.Size, part, keep, settle)
}

// part makes a file for a song of the stream st to come into, or returns
// nil if the cache would not keep it. Each has a name of its own: the same
// song played again while it still comes in has another.
func (c *Cache) part(st *netease.Stream) *os.File {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	limit, dir := c.limit, c.dir
	c.mu.Unlock()
	// A song of more than a quarter of it would push out too many.
	if limit <= 0 || st.Size > limit/4 {
		return nil
	}
	if os.MkdirAll(dir, 0o700) != nil {
		return nil
	}
	f, err := os.CreateTemp(dir, "*.part")
	if err != nil {
		return nil
	}
	return f
}

func (c *Cache) keeper(id int64, st *netease.Stream) (func(string, int64) (string, bool), func(string)) {
	return c.keeperFor(id, "", st)
}

func (c *Cache) keeperFor(id int64, reqLevel string, st *netease.Stream) (func(string, int64) (string, bool), func(string)) {
	return func(part string, n int64) (string, bool) {
		return c.keepFor(id, reqLevel, st, part, n)
	}, c.settled
}

func (c *Cache) settled(path string) {
	c.forget(path)
	if c.kick() {
		go c.pruneLoop()
	}
}

// keep keeps the song of the stream st, n bytes, from the file part, which
// is closed. It returns where the song is: kept, or still at part. The
// caller settles the file once it has opened it.
func (c *Cache) keep(id int64, st *netease.Stream, part string, n int64) (string, bool) {
	return c.keepFor(id, "", st, part, n)
}

func (c *Cache) keepFor(id int64, reqLevel string, st *netease.Stream, part string, n int64) (string, bool) {
	c.mu.Lock()
	limit, dir := c.limit, c.dir
	c.mu.Unlock()
	if n > limit/4 {
		return part, false
	}
	kept := *st
	if kept.Type == "" {
		kept.Type = "mp3"
	}
	kept.Size = n
	if !keepable(kept.Level, kept.Type) {
		return part, false
	}
	key := cacheKey(id, kept.Level)
	metaPath := filepath.Join(dir, key+".json")
	cm := cacheMeta{Stream: kept}
	if prevRaw, err := os.ReadFile(metaPath); err == nil {
		var prev cacheMeta
		if json.Unmarshal(prevRaw, &prev) == nil && prev.MaxRequested != "" && prev.CheckedAt > 0 && time.Since(time.Unix(prev.CheckedAt, 0)) <= qualityHintTTL {
			cm.MaxRequested, cm.CheckedAt = prev.MaxRequested, prev.CheckedAt
		}
	}
	if levelIndex(reqLevel) > levelIndex(kept.Level) {
		cm.MaxRequested = higherLevel(cm.MaxRequested, reqLevel)
		cm.CheckedAt = time.Now().Unix()
	}
	meta, _ := json.Marshal(cm)
	path := filepath.Join(dir, key+"."+kept.Type)
	// keep is called with the download lock held, and the decoder waits
	// on that lock. Protect the name before it exists, then make sure a
	// clean starts after the file is there: one may already have looked.
	c.protect(path)
	if os.Rename(part, path) != nil {
		// On Windows, renaming an existing .part over an existing cached copy
		// fails while another reader (such as a fading tail or loudness meter)
		// still has it open. If part itself exists with size n and the
		// destination already holds the complete file of the same size, reuse
		// it and still persist the updated metadata.
		pInfo, perr := os.Stat(part)
		existing, serr := os.Stat(path)
		if perr == nil && pInfo.Size() == n && serr == nil && existing.Size() == n {
			os.Remove(part)
		} else {
			c.forget(path)
			return part, false
		}
	}
	os.WriteFile(metaPath, meta, 0o600)
	if c.kick() {
		go c.pruneLoop()
	}
	return path, true
}

// ResetQualityHints clears downgraded quality aliases on cached songs so
// higher-tier requests (e.g. after upgrading to VIP) re-check NetEase.
func (c *Cache) ResetQualityHints() {
	if c == nil {
		return
	}
	dir := c.Dir()
	if dir == "" {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".json" {
			continue
		}
		path := filepath.Join(dir, e.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var meta cacheMeta
		if json.Unmarshal(raw, &meta) != nil || meta.MaxRequested == "" {
			continue
		}
		meta.MaxRequested, meta.CheckedAt = "", 0
		if updated, err := json.Marshal(meta); err == nil {
			os.WriteFile(path, updated, 0o600)
		}
	}
}

// Size returns how many bytes the songs kept take.
func (c *Cache) Size() int64 {
	var n int64
	for _, f := range songsIn(c.Dir()) {
		n += f.size
	}
	return n
}

// Clear drops every song kept.
func (c *Cache) Clear() {
	for _, f := range songsIn(c.Dir()) {
		c.remove(f.path)
	}
}

type cachedSong struct {
	path string
	size int64
	used time.Time
}

// songsIn lists the songs kept in dir.
func songsIn(dir string) []cachedSong {
	if dir == "" {
		return nil
	}
	entries, _ := os.ReadDir(dir)
	var out []cachedSong
	for _, e := range entries {
		ext := filepath.Ext(e.Name())
		if ext != ".mp3" && ext != ".flac" {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, cachedSong{filepath.Join(dir, e.Name()), info.Size(), info.ModTime()})
	}
	return out
}

func (c *Cache) remove(path string) {
	os.Remove(path)
	os.Remove(strings.TrimSuffix(path, filepath.Ext(path)) + ".json")
}

func (c *Cache) protect(path string) {
	c.mu.Lock()
	c.fresh[path]++
	c.mu.Unlock()
}

func (c *Cache) forget(path string) {
	c.mu.Lock()
	if c.fresh[path] > 1 {
		c.fresh[path]--
	} else {
		delete(c.fresh, path)
	}
	c.cond.Broadcast()
	c.mu.Unlock()
}

// kick queues a clean and reports whether this call must start it.
func (c *Cache) kick() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dirty = true
	c.cond.Broadcast()
	if c.running {
		return false
	}
	c.running = true
	return true
}

// pruneWait runs a clean to the end. Callers that change the limit or the
// directory expect songs over the limit to be gone before they return.
func (c *Cache) pruneWait() {
	c.mu.Lock()
	c.dirty = true
	c.waiting++
	start := !c.running
	if start {
		c.running = true
		c.mu.Unlock()
		c.pruneLoop()
		c.mu.Lock()
	} else {
		c.cond.Broadcast()
		for c.running {
			c.cond.Wait()
		}
	}
	c.waiting--
	c.mu.Unlock()
}

// pruneLoop drops what is over the limit away from the download lock.
// A pass keeps songs still being handed to playback. Work that arrives
// during the pass is done again; otherwise it waits for that hand-off.
func (c *Cache) pruneLoop() {
	for {
		c.mu.Lock()
		dir, limit := c.dir, c.limit
		c.dirty = false
		c.mu.Unlock()
		c.removeOver(dir, limit)
		c.mu.Lock()
		if c.dirty {
			c.mu.Unlock()
			continue
		}
		if len(c.fresh) > 0 && c.waiting == 0 {
			c.cond.Wait()
			c.mu.Unlock()
			continue
		}
		c.running = false
		c.cond.Broadcast()
		c.mu.Unlock()
		return
	}
}

// removeOver drops the songs played longest ago until the rest fit the limit.
func (c *Cache) removeOver(dir string, limit int64) {
	if dir == "" {
		return
	}
	dropParts(dir, time.Hour)
	songs := songsIn(dir)
	if hook := pruneHook.Load(); hook != nil {
		(*hook)()
	}
	slices.SortFunc(songs, func(a, b cachedSong) int { return a.used.Compare(b.used) })
	var total int64
	for _, f := range songs {
		total += f.size
	}
	for _, f := range songs {
		if total <= limit {
			return
		}
		c.mu.Lock()
		_, fresh := c.fresh[f.path]
		if !fresh {
			c.remove(f.path)
			total -= f.size
		}
		c.mu.Unlock()
	}
}

// dropParts removes the files in dir of songs that never all came in, left
// by an app that quit as they came, untouched for longer than idle: one
// coming in is written to every moment.
func dropParts(dir string, idle time.Duration) {
	dropPartsLike(dir, "*.part", idle)
}

// streamSpools is the name of the temporary files of songs the cache does
// not keep, in the system's temporary folder.
const streamSpools = "cloudy-stream-*.part"

// DropStreamSpools removes the temporary files of songs streamed without
// being kept that an app which crashed, or was killed, left behind: those
// untouched for an hour. One still playing is in use, and stays.
func DropStreamSpools() {
	dropPartsLike(os.TempDir(), streamSpools, time.Hour)
}

func dropPartsLike(dir, pattern string, idle time.Duration) {
	if dir == "" {
		return
	}
	parts, _ := filepath.Glob(filepath.Join(dir, pattern))
	for _, path := range parts {
		if info, err := os.Stat(path); err == nil && time.Since(info.ModTime()) > idle {
			os.Remove(path)
		}
	}
}
