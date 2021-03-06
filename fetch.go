// +build !windows,!netbsd,!openbsd,!nofuse !noftp

package main

import (
	"errors"
	"flag"
	"io"
	"log"
	"strings"
	"sync"
	"time"

	humanize "github.com/dustin/go-humanize"
	"golang.org/x/net/context"
)

var (
	localCacheDir       = flag.String("local_cache_dir", "%rufs_var_storage%/cache/", "Where to store local cache")
	localCacheSize      = flag.String("local_cache_size", "20G", "How big the local cache can be")
	fetchBlockSizePower = flag.Uint("fetch_block_size_power", 16, "Will fetch all data in blocks of 2^<value>.")
	prefetchBlocks      = flag.Uint("prefetch_blocks", 0, "Prefetch this number of blocks")

	fetchers = map[*Server]*Fetcher{}

	ErrInterrupted = errors.New("ErrInterrupted")
)

type Fetcher struct {
	server         *Server
	peers          map[string]*pfPeer
	blockSize      int
	localCacheDir  string
	localCacheSize uint64
}

func GetFetcher(server *Server) (*Fetcher, error) {
	if f, found := fetchers[server]; found {
		return f, nil
	}
	lcs, err := humanize.ParseBytes(*localCacheSize)
	if err != nil {
		return nil, err
	}
	f := &Fetcher{
		server:         server,
		peers:          map[string]*pfPeer{},
		blockSize:      1 << *fetchBlockSizePower,
		localCacheDir:  getPath(*localCacheDir),
		localCacheSize: lcs,
	}
	fetchers[server] = f
	return f, nil
}

func (f *Fetcher) Setup() error {
	return nil
}

func (f *Fetcher) Run(done <-chan void) error {
	<-done
	return nil
}

type pfPeer struct {
	ident   string
	client  *RUFSClient
	latency *time.Duration
}

func (f *Fetcher) NewPeer(ident string) (*pfPeer, error) {
	ex := strings.Split(ident, "@")
	tlsCfg := getTlsConfig(TlsConfigServerClient, f.server.ca, f.server.cert, ex[0])
	client, err := NewRUFSClient(ex[1], tlsCfg)
	if err != nil {
		return nil, err
	}
	r := &pfPeer{
		ident:  ident,
		client: client,
	}
	r.MeasureLatency()
	return r, nil
}

func (r *pfPeer) MeasureLatency() error {
	// Do an initial ping to warm up the peer, get rufs into memory cache etc
	if err := r.client.Ping(); err != nil {
		r.latency = nil
		return err
	}
	t0 := time.Now()
	if err := r.client.Ping(); err != nil {
		r.latency = nil
		return err
	}
	t := time.Now().Sub(t0)
	log.Printf("Latency to %s: %s", r.ident, t)
	r.latency = &t
	return nil
}

type pfHandle struct {
	fetcher  *Fetcher
	hash     string
	size     int64
	peers    []string
	cache    [][]byte
	active   []chan void
	cacheMtx sync.Mutex
	lastErr  error
}

func (f *Fetcher) NewHandle(hash string, size int64, peers []string) (*pfHandle, error) {
	// TODO: Dedupe by hash
	blocks := size/int64(f.blockSize) + 1
	h := &pfHandle{
		fetcher: f,
		hash:    hash,
		size:    size,
		peers:   peers,
		cache:   make([][]byte, blocks),
		active:  make([]chan void, blocks),
	}
	found := false
	for _, p := range peers {
		if _, ok := f.peers[p]; ok {
			found = true
		} else {
			r, err := f.NewPeer(p)
			if err == nil {
				f.peers[p] = r
				found = true
			}
		}
	}
	if !found {
		return nil, errors.New("didn't find peer with file")
	}
	return h, nil
}

func (h *pfHandle) fetch(offset int64) {
	bs := int64(h.fetcher.blockSize)
	b := int64(offset / bs)
	h.active[b] = make(chan void)
	go func() {
		buf, err := h.realFetch(offset)
		log.Printf("Fetched block %d", offset)
		h.cacheMtx.Lock()
		if err != nil {
			h.lastErr = err
		} else {
			h.cache[b] = buf
		}
		close(h.active[b])
		h.cacheMtx.Unlock()
		log.Printf("Processed block %d", offset)
	}()
}

func (h *pfHandle) realFetch(offset int64) ([]byte, error) {
	retErr := errors.New("Couldn't find peer with file")
	for _, p := range h.peers {
		r, ok := h.fetcher.peers[p]
		if !ok {
			continue
		}
		ret, err := r.client.Read(h.hash, offset, h.fetcher.blockSize)
		if err != nil {
			retErr = err
			continue
		}
		return ret.Data, nil
	}
	return nil, retErr
}

func (h *pfHandle) Close() {
}

func (h *pfHandle) Read(ctx context.Context, offset int64, size int) ([]byte, error) {
	if offset >= h.size {
		return nil, nil
	}
	bs := h.fetcher.blockSize
	bs64 := int64(bs)
	var blocks []int64
	for b := offset / bs64; int64((offset+int64(size+bs)-1)/bs64) > b; b++ {
		blocks = append(blocks, b*bs64)
	}
	log.Printf("pfHandle->Read: %v", blocks)
	var unfetched []chan void
	h.cacheMtx.Lock()
	for _, o := range blocks {
		b := o / bs64
		if h.cache[b] == nil {
			if h.active[b] == nil {
				h.fetch(o)
			}
			unfetched = append(unfetched, h.active[b])
		}
	}
	// Ugly-ass purging
	if b := blocks[0]/bs64 - 1; b > 0 && h.cache[b] != nil {
		h.cache[b] = nil
		h.active[b] = nil
	}
	// prefetch
	for i := uint(0); *prefetchBlocks > i; i++ {
		b := blocks[len(blocks)-1]/bs64 + 1 + int64(i)
		if b*bs64 < h.size && h.cache[b] == nil && h.active[b] == nil {
			h.fetch(b * bs64)
		}
	}
	h.cacheMtx.Unlock()
	log.Printf("pfHandle->Read: Missing %d block(s)", len(unfetched))
	for _, c := range unfetched {
		log.Println("Waiting for block...")
		select {
		case <-ctx.Done():
			return nil, ErrInterrupted
		case <-c:
		}
	}
	log.Println("Got all blocks, grabbing...")
	var bufs [][]byte
	h.cacheMtx.Lock()
	for _, o := range blocks {
		b := int64(o / bs64)
		if h.cache[b] == nil {
			h.cacheMtx.Unlock()
			return nil, h.lastErr
		}
		bufs = append(bufs, h.cache[b])
	}
	h.cacheMtx.Unlock()
	bo := offset - blocks[0]
	if cut := int((bo + int64(size)) % bs64); cut > 0 {
		l := len(bufs) - 1
		if len(bufs[l]) > cut {
			log.Printf("Cutting off %d at %d", l, cut)
			bufs[l] = bufs[l][:cut]
		}
	}
	if bo > 0 {
		log.Printf("Block offset %d", bo)
		bufs[0] = bufs[0][bo:]
	}
	var ret []byte
	if len(bufs) == 1 {
		ret = bufs[0]
	} else {
		ret = make([]byte, 0, size)
		for _, b := range bufs {
			ret = append(ret, b...)
		}
	}
	if len(ret) > size {
		log.Printf("Returning %d bytes instead of %d (%d too much)", len(ret), size, len(ret)-size)
		// ret = ret[:size]
	}
	return ret, nil
}

type pfStream struct {
	h      *pfHandle
	offset int64
	ctx    context.Context
	cancel func()
}

func (h *pfHandle) Stream(offset int64) io.ReadCloser {
	ctx, cancel := context.WithCancel(context.Background())
	return &pfStream{h, offset, ctx, cancel}
}

func (s *pfStream) Read(p []byte) (n int, err error) {
	data, err := s.h.Read(s.ctx, s.offset, len(p))
	if err != nil {
		return 0, err
	}
	copy(p, data)
	n = len(data)
	s.offset += int64(n)
	if n < len(p) {
		err = io.EOF
	}
	return
}

func (s *pfStream) Close() error {
	s.cancel()
	s.h.Close()
	return nil
}
