package downloader

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/logger"
	"github.com/ethereum/go-ethereum/logger/glog"
)

const (
	maxBlockFetch    = 128              // Amount of max blocks to be fetched per chunk
	peerCountTimeout = 12 * time.Second // Amount of time it takes for the peer handler to ignore minDesiredPeerCount
	hashTtl          = 20 * time.Second // The amount of time it takes for a hash request to time out
)

var (
	minDesiredPeerCount = 5                // Amount of peers desired to start syncing
	blockTtl            = 20 * time.Second // The amount of time it takes for a block request to time out

	errLowTd               = errors.New("peer's TD is too low")
	ErrBusy                = errors.New("busy")
	errUnknownPeer         = errors.New("peer's unknown or unhealthy")
	errBadPeer             = errors.New("action from bad peer ignored")
	errNoPeers             = errors.New("no peers to keep download active")
	ErrPendingQueue        = errors.New("pending items in queue")
	ErrTimeout             = errors.New("timeout")
	errEmptyHashSet        = errors.New("empty hash set by peer")
	errPeersUnavailable    = errors.New("no peers available or all peers tried for block download process")
	errAlreadyInPool       = errors.New("hash already in pool")
	errBlockNumberOverflow = errors.New("received block which overflows")
	errCancelHashFetch     = errors.New("hash fetching cancelled (requested)")
	errCancelBlockFetch    = errors.New("block downloading cancelled (requested)")
	errNoSyncActive        = errors.New("no sync active")
)

type hashCheckFn func(common.Hash) bool
type getBlockFn func(common.Hash) *types.Block
type chainInsertFn func(types.Blocks) (int, error)
type hashIterFn func() (common.Hash, error)

type blockPack struct {
	peerId string
	blocks []*types.Block
}

type hashPack struct {
	peerId string
	hashes []common.Hash
}

type Downloader struct {
	mu    sync.RWMutex
	queue *queue
	peers *peerSet

	// Callbacks
	hasBlock hashCheckFn
	getBlock getBlockFn

	// Status
	synchronising int32

	// Channels
	newPeerCh chan *peer
	hashCh    chan hashPack
	blockCh   chan blockPack
	cancelCh  chan struct{}
}

func New(hasBlock hashCheckFn, getBlock getBlockFn) *Downloader {
	downloader := &Downloader{
		queue:     newQueue(),
		peers:     newPeerSet(),
		hasBlock:  hasBlock,
		getBlock:  getBlock,
		newPeerCh: make(chan *peer, 1),
		hashCh:    make(chan hashPack, 1),
		blockCh:   make(chan blockPack, 1),
	}

	return downloader
}

func (d *Downloader) Stats() (current int, max int) {
	return d.queue.Size()
}

// RegisterPeer injects a new download peer into the set of block source to be
// used for fetching hashes and blocks from.
func (d *Downloader) RegisterPeer(id string, head common.Hash, getHashes hashFetcherFn, getBlocks blockFetcherFn) error {
	glog.V(logger.Detail).Infoln("Registering peer", id)
	if err := d.peers.Register(newPeer(id, head, getHashes, getBlocks)); err != nil {
		glog.V(logger.Error).Infoln("Register failed:", err)
		return err
	}
	return nil
}

// UnregisterPeer remove a peer from the known list, preventing any action from
// the specified peer.
func (d *Downloader) UnregisterPeer(id string) error {
	glog.V(logger.Detail).Infoln("Unregistering peer", id)
	if err := d.peers.Unregister(id); err != nil {
		glog.V(logger.Error).Infoln("Unregister failed:", err)
		return err
	}
	return nil
}

// Synchronise will select the peer and use it for synchronising. If an empty string is given
// it will use the best peer possible and synchronize if it's TD is higher than our own. If any of the
// checks fail an error will be returned. This method is synchronous
func (d *Downloader) Synchronise(id string, hash common.Hash) error {
	// Make sure only one goroutine is ever allowed past this point at once
	if !atomic.CompareAndSwapInt32(&d.synchronising, 0, 1) {
		return ErrBusy
	}
	defer atomic.StoreInt32(&d.synchronising, 0)

	// Create cancel channel for aborting midflight
	d.cancelCh = make(chan struct{})

	// Abort if the queue still contains some leftover data
	if _, cached := d.queue.Size(); cached > 0 && d.queue.GetHeadBlock() != nil {
		return ErrPendingQueue
	}
	// Reset the queue and peer set to clean any internal leftover state
	d.queue.Reset()
	d.peers.Reset()

	// Retrieve the origin peer and initiate the downloading process
	p := d.peers.Peer(id)
	if p == nil {
		return errUnknownPeer
	}
	return d.syncWithPeer(p, hash)
}

// TakeBlocks takes blocks from the queue and yields them to the blockTaker handler
// it's possible it yields no blocks
func (d *Downloader) TakeBlocks() types.Blocks {
	// Check that there are blocks available and its parents are known
	head := d.queue.GetHeadBlock()
	if head == nil || !d.hasBlock(head.ParentHash()) {
		return nil
	}
	// Retrieve a full batch of blocks
	return d.queue.TakeBlocks(head)
}

func (d *Downloader) Has(hash common.Hash) bool {
	return d.queue.Has(hash)
}

// syncWithPeer starts a block synchronization based on the hash chain from the
// specified peer and head hash.
func (d *Downloader) syncWithPeer(p *peer, hash common.Hash) (err error) {
	defer func() {
		// reset on error
		if err != nil {
			d.queue.Reset()
		}
	}()

	glog.V(logger.Debug).Infoln("Synchronizing with the network using:", p.id)
	if err = d.fetchHashes(p, hash); err != nil {
		return err
	}
	if err = d.fetchBlocks(); err != nil {
		return err
	}
	glog.V(logger.Debug).Infoln("Synchronization completed")

	return nil
}

// Cancel cancels all of the operations and resets the queue. It returns true
// if the cancel operation was completed.
func (d *Downloader) Cancel() bool {
	hs, bs := d.queue.Size()
	// If we're not syncing just return.
	if atomic.LoadInt32(&d.synchronising) == 0 && hs == 0 && bs == 0 {
		return false
	}

	close(d.cancelCh)

	// clean up
hashDone:
	for {
		select {
		case <-d.hashCh:
		default:
			break hashDone
		}
	}

blockDone:
	for {
		select {
		case <-d.blockCh:
		default:
			break blockDone
		}
	}

	// reset the queue
	d.queue.Reset()

	return true
}

// XXX Make synchronous
func (d *Downloader) fetchHashes(p *peer, h common.Hash) error {
	glog.V(logger.Debug).Infof("Downloading hashes (%x) from %s", h[:4], p.id)

	start := time.Now()

	// Add the hash to the queue first
	d.queue.Insert([]common.Hash{h})

	// Get the first batch of hashes
	p.getHashes(h)

	var (
		failureResponseTimer = time.NewTimer(hashTtl)
		attemptedPeers       = make(map[string]bool) // attempted peers will help with retries
		activePeer           = p                     // active peer will help determine the current active peer
		hash                 common.Hash             // common and last hash
	)
	attemptedPeers[p.id] = true

out:
	for {
		select {
		case <-d.cancelCh:
			return errCancelHashFetch
		case hashPack := <-d.hashCh:
			// Make sure the active peer is giving us the hashes
			if hashPack.peerId != activePeer.id {
				glog.V(logger.Debug).Infof("Received hashes from incorrect peer(%s)\n", hashPack.peerId)
				break
			}

			failureResponseTimer.Reset(hashTtl)

			// Make sure the peer actually gave something valid
			if len(hashPack.hashes) == 0 {
				glog.V(logger.Debug).Infof("Peer (%s) responded with empty hash set\n", activePeer.id)
				d.queue.Reset()

				return errEmptyHashSet
			}
			// Determine if we're done fetching hashes (queue up all pending), and continue if not done
			done, index := false, 0
			for index, hash = range hashPack.hashes {
				if d.hasBlock(hash) || d.queue.GetBlock(hash) != nil {
					glog.V(logger.Debug).Infof("Found common hash %x\n", hash[:4])
					hashPack.hashes = hashPack.hashes[:index]
					done = true
					break
				}
			}
			d.queue.Insert(hashPack.hashes)

			if !done {
				activePeer.getHashes(hash)
				continue
			}
			// We're done, allocate the download cache and proceed pulling the blocks
			offset := 0
			if block := d.getBlock(hash); block != nil {
				offset = int(block.NumberU64() + 1)
			}
			d.queue.Alloc(offset)
			break out

		case <-failureResponseTimer.C:
			glog.V(logger.Debug).Infof("Peer (%s) didn't respond in time for hash request\n", p.id)

			var p *peer // p will be set if a peer can be found
			// Attempt to find a new peer by checking inclusion of peers best hash in our
			// already fetched hash list. This can't guarantee 100% correctness but does
			// a fair job. This is always either correct or false incorrect.
			for _, peer := range d.peers.AllPeers() {
				if d.queue.Has(peer.head) && !attemptedPeers[peer.id] {
					p = peer
					break
				}
			}
			// if all peers have been tried, abort the process entirely or if the hash is
			// the zero hash.
			if p == nil || (hash == common.Hash{}) {
				d.queue.Reset()
				return ErrTimeout
			}
			// set p to the active peer. this will invalidate any hashes that may be returned
			// by our previous (delayed) peer.
			activePeer = p
			p.getHashes(hash)
			glog.V(logger.Debug).Infof("Hash fetching switched to new peer(%s)\n", p.id)
		}
	}
	glog.V(logger.Debug).Infof("Downloaded hashes (%d) in %v\n", d.queue.Pending(), time.Since(start))

	return nil
}

// fetchBlocks iteratively downloads the entire schedules block-chain, taking
// any available peers, reserving a chunk of blocks for each, wait for delivery
// and periodically checking for timeouts.
func (d *Downloader) fetchBlocks() error {
	glog.V(logger.Debug).Infoln("Downloading", d.queue.Pending(), "block(s)")
	start := time.Now()

	// default ticker for re-fetching blocks every now and then
	ticker := time.NewTicker(20 * time.Millisecond)
out:
	for {
		select {
		case <-d.cancelCh:
			return errCancelBlockFetch
		case blockPack := <-d.blockCh:
			// If the peer was previously banned and failed to deliver it's pack
			// in a reasonable time frame, ignore it's message.
			if peer := d.peers.Peer(blockPack.peerId); peer != nil {
				// Deliver the received chunk of blocks, but drop the peer if invalid
				if err := d.queue.Deliver(blockPack.peerId, blockPack.blocks); err != nil {
					glog.V(logger.Debug).Infof("Failed delivery for peer %s: %v\n", blockPack.peerId, err)
					peer.Demote()
					break
				}
				if glog.V(logger.Debug) {
					glog.Infof("Added %d blocks from: %s\n", len(blockPack.blocks), blockPack.peerId)
				}
				// Promote the peer and update it's idle state
				peer.Promote()
				peer.SetIdle()
			}
		case <-ticker.C:
			// Check for bad peers. Bad peers may indicate a peer not responding
			// to a `getBlocks` message. A timeout of 5 seconds is set. Peers
			// that badly or poorly behave are removed from the peer set (not banned).
			// Bad peers are excluded from the available peer set and therefor won't be
			// reused. XXX We could re-introduce peers after X time.
			badPeers := d.queue.Expire(blockTtl)
			for _, pid := range badPeers {
				// XXX We could make use of a reputation system here ranking peers
				// in their performance
				// 1) Time for them to respond;
				// 2) Measure their speed;
				// 3) Amount and availability.
				if peer := d.peers.Peer(pid); peer != nil {
					peer.Demote()
				}
			}
			// After removing bad peers make sure we actually have sufficient peer left to keep downloading
			if d.peers.Len() == 0 {
				d.queue.Reset()
				return errNoPeers
			}
			// If there are unrequested hashes left start fetching
			// from the available peers.
			if d.queue.Pending() > 0 {
				// Throttle the download if block cache is full and waiting processing
				if d.queue.Throttle() {
					continue
				}
				// Send a download request to all idle peers, until throttled
				idlePeers := d.peers.IdlePeers()
				for _, peer := range idlePeers {
					// Short circuit if throttling activated since above
					if d.queue.Throttle() {
						break
					}
					// Get a possible chunk. If nil is returned no chunk
					// could be returned due to no hashes available.
					request := d.queue.Reserve(peer, maxBlockFetch)
					if request == nil {
						continue
					}
					// Fetch the chunk and check for error. If the peer was somehow
					// already fetching a chunk due to a bug, it will be returned to
					// the queue
					if err := peer.Fetch(request); err != nil {
						glog.V(logger.Error).Infof("Peer %s received double work\n", peer.id)
						d.queue.Cancel(request)
					}
				}
				// Make sure that we have peers available for fetching. If all peers have been tried
				// and all failed throw an error
				if d.queue.InFlight() == 0 {
					d.queue.Reset()

					return fmt.Errorf("%v peers available = %d. total peers = %d. hashes needed = %d", errPeersUnavailable, len(idlePeers), d.peers.Len(), d.queue.Pending())
				}

			} else if d.queue.InFlight() == 0 {
				// When there are no more queue and no more in flight, We can
				// safely assume we're done. Another part of the process will  check
				// for parent errors and will re-request anything that's missing
				break out
			}
		}
	}
	glog.V(logger.Detail).Infoln("Downloaded block(s) in", time.Since(start))

	return nil
}

// DeliverBlocks injects a new batch of blocks received from a remote node.
// This is usually invoked through the BlocksMsg by the protocol handler.
func (d *Downloader) DeliverBlocks(id string, blocks []*types.Block) error {
	// Make sure the downloader is active
	if atomic.LoadInt32(&d.synchronising) == 0 {
		return errNoSyncActive
	}
	d.blockCh <- blockPack{id, blocks}

	return nil
}

// DeliverHashes injects a new batch of hashes received from a remote node into
// the download schedule. This is usually invoked through the BlockHashesMsg by
// the protocol handler.
func (d *Downloader) DeliverHashes(id string, hashes []common.Hash) error {
	// Make sure the downloader is active
	if atomic.LoadInt32(&d.synchronising) == 0 {
		return errNoSyncActive
	}
	if glog.V(logger.Debug) && len(hashes) != 0 {
		from, to := hashes[0], hashes[len(hashes)-1]
		glog.V(logger.Debug).Infof("adding %d (T=%d) hashes [ %x / %x ] from: %s\n", len(hashes), d.queue.Pending(), from[:4], to[:4], id)
	}
	d.hashCh <- hashPack{id, hashes}

	return nil
}
