package ipfscluster

import (
	"errors"
	"fmt"

	cid "github.com/ipfs/go-cid"
	peer "github.com/libp2p/go-libp2p-peer"

	"github.com/ipfs/ipfs-cluster/api"
)

// This file gathers allocation logic used when pinning or re-pinning
// to find which peers should be allocated to a Cid. Allocation is constrained
// by ReplicationFactorMin and ReplicationFactorMax parametres obtained
// from the Pin object.

//The allocation
// process has several steps:
//
// * Find which peers are pinning a CID
// * Obtain the last values for the configured informer metrics from the
//   monitor component
// * Divide the metrics between "current" (peers already pinning the CID)
//   and "candidates" (peers that could pin the CID), as long as their metrics
//   are valid.
// * Given the candidates:
//   * Check if we are overpinning an item
//   * Check if there are not enough candidates for the "needed" replication
//     factor.
//   * If there are enough candidates:
//     * Call the configured allocator, which sorts the candidates (and
//       may veto some depending on the allocation strategy.
//     * The allocator returns a list of final candidate peers sorted by
//       order of preference.
//     * Take as many final candidates from the list as we can, until
//       ReplicationFactorMax is reached. Error if there are less than
//       ReplicationFactorMin.

// allocate finds peers to allocate a hash using the informer and the monitor
// it should only be used with valid replicationFactors (rplMin and rplMax
// which are positive and rplMin <= rplMax).
// It only returns new allocations when needed. nil, nil means current
// are ok.
func (c *Cluster) allocate(hash *cid.Cid, rplMin, rplMax int, blacklist []peer.ID) ([]peer.ID, error) {
	// Figure out who is holding the CID
	currentAllocs := c.getCurrentAllocations(hash)
	metrics, err := c.getInformerMetrics()
	if err != nil {
		return nil, err
	}

	currentMetrics := make(map[peer.ID]api.Metric)
	candidatesMetrics := make(map[peer.ID]api.Metric)

	// Divide metrics between current and candidates.
	for _, m := range metrics {
		switch {
		case m.Discard() || containsPeer(blacklist, m.Peer):
			// discard peers with invalid metrics and
			// those in the blacklist
			continue
		case containsPeer(currentAllocs, m.Peer):
			currentMetrics[m.Peer] = m
		default:
			candidatesMetrics[m.Peer] = m
		}
	}

	return c.obtainAllocations(hash,
		rplMin,
		rplMax,
		currentMetrics,
		candidatesMetrics)
}

// getCurrentAllocations returns the list of peers allocated to a Cid.
func (c *Cluster) getCurrentAllocations(h *cid.Cid) []peer.ID {
	var allocs []peer.ID
	st, err := c.consensus.State()
	if err != nil {
		// no state we assume it is empty. If there was other
		// problem, we would fail to commit anyway.
		allocs = []peer.ID{}
	} else {
		pin := st.Get(h)
		allocs = pin.Allocations
	}
	return allocs
}

// getInformerMetrics returns the MonitorLastMetrics() for the
// configured informer.
func (c *Cluster) getInformerMetrics() ([]api.Metric, error) {
	var metrics []api.Metric
	metricName := c.informer.Name()
	l, err := c.consensus.Leader()
	if err != nil {
		return nil, errors.New("cannot determine leading Monitor")
	}

	err = c.rpcClient.Call(l,
		"Cluster", "PeerMonitorLastMetrics",
		metricName,
		&metrics)
	if err != nil {
		return nil, err
	}
	return metrics, nil
}

// allocationError logs an allocation error
func allocationError(hash *cid.Cid, needed, wanted int, candidatesValid []peer.ID) error {
	logger.Errorf("Not enough candidates to allocate %s:", hash)
	logger.Errorf("  Needed: %d", needed)
	logger.Errorf("  Wanted: %d", wanted)
	logger.Errorf("  Valid candidates: %d:", len(candidatesValid))
	for _, c := range candidatesValid {
		logger.Errorf("    - %s", c.Pretty())
	}
	errorMsg := "not enough peers to allocate CID. "
	errorMsg += fmt.Sprintf("Needed at least: %d. ", needed)
	errorMsg += fmt.Sprintf("Wanted at most: %d. ", wanted)
	errorMsg += fmt.Sprintf("Valid candidates: %d. ", len(candidatesValid))
	errorMsg += "See logs for more info."
	return errors.New(errorMsg)
}

func (c *Cluster) obtainAllocations(
	hash *cid.Cid,
	rplMin, rplMax int,
	currentValidMetrics, candidatesMetrics map[peer.ID]api.Metric) ([]peer.ID, error) {

	// The list of peers in current
	validAllocations := make([]peer.ID, 0, len(currentValidMetrics))
	for k := range currentValidMetrics {
		validAllocations = append(validAllocations, k)
	}

	nCurrentValid := len(validAllocations)
	nCandidatesValid := len(candidatesMetrics)
	needed := rplMin - nCurrentValid // The minimum we need
	wanted := rplMax - nCurrentValid // The maximum we want

	logger.Debugf("obtainAllocations: current valid: %d", nCurrentValid)
	logger.Debugf("obtainAllocations: candidates valid: %d", nCandidatesValid)
	logger.Debugf("obtainAllocations: Needed: %d", needed)
	logger.Debugf("obtainAllocations: Wanted: %d", wanted)

	// Reminder: rplMin <= rplMax AND >0

	if wanted <= 0 { // alocations above maximum threshold: drop some
		// This could be done more intelligently by dropping them
		// according to the allocator order (i.e. free-ing peers
		// with most used space first).
		return validAllocations[0 : len(validAllocations)+wanted], nil
	}

	if needed <= 0 { // allocations are above minimal threshold
		// We keep things as they are. Avoid any changes to the pin set.
		return nil, nil
	}

	if nCandidatesValid < needed { // not enough candidates
		candidatesValid := []peer.ID{}
		for k := range candidatesMetrics {
			candidatesValid = append(candidatesValid, k)
		}
		return nil, allocationError(hash, needed, wanted, candidatesValid)
	}

	// We can allocate from this point. Use the allocator to decide
	// on the priority of candidates grab as many as "wanted"

	// the allocator returns a list of peers ordered by priority
	finalAllocs, err := c.allocator.Allocate(
		hash, currentValidMetrics, candidatesMetrics)
	if err != nil {
		return nil, logError(err.Error())
	}

	logger.Debugf("obtainAllocations: allocate(): %s", finalAllocs)

	// check that we have enough as the allocator may have returned
	// less candidates than provided.
	if got := len(finalAllocs); got < needed {
		return nil, allocationError(hash, needed, wanted, finalAllocs)
	}

	allocationsToUse := minInt(wanted, len(finalAllocs))

	// the final result is the currently valid allocations
	// along with the ones provided by the allocator
	return append(validAllocations, finalAllocs[0:allocationsToUse]...), nil
}