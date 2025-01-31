// SPDX-License-Identifier: AGPL-3.0-only
// Provenance-includes-location: https://github.com/cortexproject/cortex/blob/master/pkg/scheduler/queue/user_queues.go
// Provenance-includes-license: Apache-2.0
// Provenance-includes-copyright: The Cortex Authors.

package queue

import (
	"errors"
	"math/rand"
	"sort"
	"time"

	"github.com/grafana/mimir/pkg/util"
)

type TenantID string

const emptyTenantID = TenantID("")

type QuerierID string
type querierIDSlice []QuerierID

// Len implements sort.Interface for querierIDSlice
func (s querierIDSlice) Len() int { return len(s) }

// Swap implements sort.Interface for querierIDSlice
func (s querierIDSlice) Swap(i, j int) { s[i], s[j] = s[j], s[i] }

// Less implements sort.Interface for querierIDSlice
func (s querierIDSlice) Less(i, j int) bool { return s[i] < s[j] }

// Search method covers for sort.Search's functionality,
// as sort.Search does not allow anything interface-based or generic yet.
func (s querierIDSlice) Search(x QuerierID) int {
	return sort.Search(len(s), func(i int) bool { return s[i] >= x })
}

type tenantRequest struct {
	tenantID TenantID
	req      Request
}

type querierConn struct {
	// Number of active connections.
	connections int

	// True if the querier notified it's gracefully shutting down.
	shuttingDown bool

	// When the last connection has been unregistered.
	disconnectedAt time.Time
}

type tenantQuerierAssignments struct {
	// a tenant has many queriers
	// a tenant has *all* queriers if:
	//  - sharding is disabled (max-queriers-per-tenant=0)
	//  - or if max-queriers-per-tenant >= the number of queriers
	//
	// Tenant -> Queriers is the core relationship randomized from the shuffle shard seed.
	// The shuffle shard seed is itself consistently hashed from the tenant ID.
	// However, the most common operation is the querier asking for its next request,
	// which requires a relatively efficient lookup or check of Querier -> Tenant.
	//
	// Reshuffling is done when:
	//  - a querier connection is added or removed
	//  - it is detected during request enqueueing that a tenant's queriers
	//    were calculated from an outdated max-queriers-per-tenant value

	queriersByID map[QuerierID]*querierConn
	// Sorted list of querier ids, used when shuffle sharding queriers for tenant
	querierIDsSorted querierIDSlice

	// How long to wait before removing a querier which has got disconnected
	// but hasn't notified about a graceful shutdown.
	querierForgetDelay time.Duration

	// List of all tenants with queues, used for iteration when searching for next queue to handle.
	tenantIDOrder []TenantID
	tenantsByID   map[TenantID]*queueTenant

	// Tenant assigned querier ID set as determined by shuffle sharding.
	// If tenant querier ID set is not nil, only those queriers can handle the tenant's requests,
	// Tenant querier ID is set to nil if sharding is off or available queriers <= tenant's maxQueriers.
	tenantQuerierIDs map[TenantID]map[QuerierID]struct{}
}

type queueTenant struct {
	tenantID    TenantID
	maxQueriers int

	// seed for shuffle sharding of queriers; computed from tenantID only,
	// and is therefore consistent between different frontends.
	shuffleShardSeed int64

	// points up to tenant order to enable efficient removal
	orderIndex int
}

// queueBroker encapsulates access to tenant queues for pending requests
// and maintains consistency with the tenant-querier assignments
type queueBroker struct {
	tenantQueuesTree *TreeQueue

	tenantQuerierAssignments tenantQuerierAssignments

	maxTenantQueueSize int
}

func newQueueBroker(maxTenantQueueSize int, forgetDelay time.Duration) *queueBroker {
	return &queueBroker{
		tenantQueuesTree: NewTreeQueue("root", maxTenantQueueSize),
		tenantQuerierAssignments: tenantQuerierAssignments{
			queriersByID:       map[QuerierID]*querierConn{},
			querierIDsSorted:   nil,
			querierForgetDelay: forgetDelay,
			tenantIDOrder:      nil,
			tenantsByID:        map[TenantID]*queueTenant{},
			tenantQuerierIDs:   map[TenantID]map[QuerierID]struct{}{},
		},
		maxTenantQueueSize: maxTenantQueueSize,
	}
}

func (qb *queueBroker) isEmpty() bool {
	return qb.tenantQueuesTree.IsEmpty()
}

// enqueueRequestBack is the standard interface to enqueue requests for dispatch to queriers.
//
// Tenants and tenant-querier shuffle sharding relationships are managed internally as needed.
func (qb *queueBroker) enqueueRequestBack(request *tenantRequest, tenantMaxQueriers int) error {
	err := qb.tenantQuerierAssignments.createOrUpdateTenant(request.tenantID, tenantMaxQueriers)
	if err != nil {
		return err
	}

	queuePath := QueuePath{string(request.tenantID)}
	err = qb.tenantQueuesTree.EnqueueBackByPath(queuePath, request)
	if errors.Is(err, ErrMaxQueueLengthExceeded) {
		return errors.Join(err, ErrTooManyRequests)
	}
	return err
}

// enqueueRequestFront should only be used for re-enqueueing previously dequeued requests
// to the front of the queue when there was a failure in dispatching to a querier.
//
// max tenant queue size checks are skipped even though queue size violations
// are not expected to occur when re-enqueuing a previously dequeued request.
func (qb *queueBroker) enqueueRequestFront(request *tenantRequest, tenantMaxQueriers int) error {
	err := qb.tenantQuerierAssignments.createOrUpdateTenant(request.tenantID, tenantMaxQueriers)
	if err != nil {
		return err
	}

	queuePath := QueuePath{string(request.tenantID)}
	return qb.tenantQueuesTree.EnqueueFrontByPath(queuePath, request)
}

func (qb *queueBroker) dequeueRequestForQuerier(lastTenantIndex int, querierID QuerierID) (*tenantRequest, *queueTenant, int, error) {
	tenant, tenantIndex, err := qb.tenantQuerierAssignments.getNextTenantForQuerier(lastTenantIndex, querierID)
	if tenant == nil || err != nil {
		return nil, tenant, tenantIndex, err
	}

	queuePath := QueuePath{string(tenant.tenantID)}
	queueElement := qb.tenantQueuesTree.DequeueByPath(queuePath)

	queueNodeAfterDequeue := qb.tenantQueuesTree.getNode(queuePath)
	if queueNodeAfterDequeue == nil {
		// queue node was deleted due to being empty after dequeue
		qb.tenantQuerierAssignments.removeTenant(tenant.tenantID)
	}

	var request *tenantRequest
	if queueElement != nil {
		// re-casting to same type it was enqueued as; panic would indicate a bug
		request = queueElement.(*tenantRequest)
	}

	return request, tenant, tenantIndex, nil
}

func (qb *queueBroker) addQuerierConnection(querierID QuerierID) {
	qb.tenantQuerierAssignments.addQuerierConnection(querierID)
}

func (qb *queueBroker) removeQuerierConnection(querierID QuerierID, now time.Time) {
	qb.tenantQuerierAssignments.removeQuerierConnection(querierID, now)
}

func (qb *queueBroker) notifyQuerierShutdown(querierID QuerierID) {
	qb.tenantQuerierAssignments.notifyQuerierShutdown(querierID)
}

func (qb *queueBroker) forgetDisconnectedQueriers(now time.Time) int {
	return qb.tenantQuerierAssignments.forgetDisconnectedQueriers(now)
}

// getNextTenantForQuerier gets the next tenant in the tenant order assigned to a given querier.
//
// The next tenant for the querier is obtained by rotating through the global tenant order
// starting just after the last tenant the querier received a request for, until a tenant
// is found that is assigned to the given querier according to the querier shuffle sharding.
// A newly connected querier provides lastTenantIndex of -1 in order to start at the beginning.
func (tqa *tenantQuerierAssignments) getNextTenantForQuerier(lastTenantIndex int, querierID QuerierID) (*queueTenant, int, error) {
	// check if querier is registered and is not shutting down
	if q := tqa.queriersByID[querierID]; q == nil || q.shuttingDown {
		return nil, lastTenantIndex, ErrQuerierShuttingDown
	}
	tenantOrderIndex := lastTenantIndex
	for iters := 0; iters < len(tqa.tenantIDOrder); iters++ {
		tenantOrderIndex++
		if tenantOrderIndex >= len(tqa.tenantIDOrder) {
			// Do not use modulo (e.g. i = (i + 1) % len(slice)) to wrap this index.
			// Tenant list can change size between calls and the querier provides its external view
			// of the lastTenantIndex it received, which is not updated when this list changes.
			// If the tenant list shrinks and the querier-provided lastTenantIndex exceeds the
			// length of the tenant list, wrapping via modulo would skip the beginning of the list.
			tenantOrderIndex = 0
		}

		tenantID := tqa.tenantIDOrder[tenantOrderIndex]
		if tenantID == emptyTenantID {
			continue
		}
		tenant := tqa.tenantsByID[tenantID]

		tenantQuerierSet := tqa.tenantQuerierIDs[tenantID]
		if tenantQuerierSet == nil {
			// tenant can use all queriers
			return tenant, tenantOrderIndex, nil
		} else if _, ok := tenantQuerierSet[querierID]; ok {
			// tenant is assigned this querier
			return tenant, tenantOrderIndex, nil
		}
	}

	return nil, lastTenantIndex, nil
}

func (tqa *tenantQuerierAssignments) getTenant(tenantID TenantID) (*queueTenant, error) {
	if tenantID == emptyTenantID {
		return nil, ErrInvalidTenantID
	}
	tenant := tqa.tenantsByID[tenantID]
	return tenant, nil
}

// createOrUpdateTenant creates or updates a tenant into the tenant-querier assignment state.
//
// New tenants are added to the tenant order list and tenant-querier shards are shuffled if needed.
// Existing tenants have the tenant-querier shards shuffled only if their maxQueriers has changed.
func (tqa *tenantQuerierAssignments) createOrUpdateTenant(tenantID TenantID, maxQueriers int) error {
	if tenantID == emptyTenantID {
		// empty tenantID is not allowed; "" is used for free spot
		return ErrInvalidTenantID
	}

	if maxQueriers < 0 {
		maxQueriers = 0
	}

	tenant := tqa.tenantsByID[tenantID]

	if tenant == nil {
		tenant = &queueTenant{
			tenantID: tenantID,
			// maxQueriers 0 enables a later check to trigger tenant-querier assignment
			// for new queue tenants with shuffle sharding enabled
			maxQueriers:      0,
			shuffleShardSeed: util.ShuffleShardSeed(string(tenantID), ""),
			// orderIndex set to sentinel value to indicate it is not inserted yet
			orderIndex: -1,
		}
		for i, id := range tqa.tenantIDOrder {
			if id == emptyTenantID {
				// previously removed tenant not yet cleaned up; take its place
				tenant.orderIndex = i
				tqa.tenantIDOrder[i] = tenantID
				tqa.tenantsByID[tenantID] = tenant
				break
			}
		}

		if tenant.orderIndex < 0 {
			// there were no empty spaces in tenant order; append
			tenant.orderIndex = len(tqa.tenantIDOrder)
			tqa.tenantIDOrder = append(tqa.tenantIDOrder, tenantID)
			tqa.tenantsByID[tenantID] = tenant
		}
	}

	// tenant now either retrieved or created
	if tenant.maxQueriers != maxQueriers {
		// tenant queriers need to be computed/recomputed;
		// either this is a new tenant with sharding enabled,
		// or the tenant already existed but its maxQueriers has changed
		tenant.maxQueriers = maxQueriers
		tqa.shuffleTenantQueriers(tenantID, nil)
	}
	return nil
}

func (tqa *tenantQuerierAssignments) addQuerierConnection(querierID QuerierID) {
	querier := tqa.queriersByID[querierID]
	if querier != nil {
		querier.connections++

		// Reset in case the querier re-connected while it was in the forget waiting period.
		querier.shuttingDown = false
		querier.disconnectedAt = time.Time{}

		return
	}

	// First connection from this querier.
	tqa.queriersByID[querierID] = &querierConn{connections: 1}
	tqa.querierIDsSorted = append(tqa.querierIDsSorted, querierID)
	sort.Sort(tqa.querierIDsSorted)

	tqa.recomputeTenantQueriers()
}

func (tqa *tenantQuerierAssignments) removeTenant(tenantID TenantID) {
	tenant := tqa.tenantsByID[tenantID]
	if tenant == nil {
		return
	}
	delete(tqa.tenantsByID, tenantID)
	tqa.tenantIDOrder[tenant.orderIndex] = emptyTenantID

	// Shrink tenant list if possible by removing empty tenant IDs.
	// We remove only from the end; removing from the middle would re-index all tenant IDs
	// and skip tenants when starting iteration from a querier-provided lastTenantIndex.
	// Empty tenant IDs stuck in the middle of the slice are handled
	// by replacing them when a new tenant ID arrives in the queue.
	for i := len(tqa.tenantIDOrder) - 1; i >= 0 && tqa.tenantIDOrder[i] == emptyTenantID; i-- {
		tqa.tenantIDOrder = tqa.tenantIDOrder[:i]
	}
}

func (tqa *tenantQuerierAssignments) removeQuerierConnection(querierID QuerierID, now time.Time) {
	querier := tqa.queriersByID[querierID]
	if querier == nil || querier.connections <= 0 {
		panic("unexpected number of connections for querier")
	}

	// Decrease the number of active connections.
	querier.connections--
	if querier.connections > 0 {
		return
	}

	// There no more active connections. If the forget delay is configured then
	// we can remove it only if querier has announced a graceful shutdown.
	if querier.shuttingDown || tqa.querierForgetDelay == 0 {
		tqa.removeQuerier(querierID)
		return
	}

	// No graceful shutdown has been notified yet, so we should track the current time
	// so that we'll remove the querier as soon as we receive the graceful shutdown
	// notification (if any) or once the threshold expires.
	querier.disconnectedAt = now
}

func (tqa *tenantQuerierAssignments) removeQuerier(querierID QuerierID) {
	delete(tqa.queriersByID, querierID)

	ix := tqa.querierIDsSorted.Search(querierID)
	if ix >= len(tqa.querierIDsSorted) || tqa.querierIDsSorted[ix] != querierID {
		panic("incorrect state of sorted queriers")
	}

	tqa.querierIDsSorted = append(tqa.querierIDsSorted[:ix], tqa.querierIDsSorted[ix+1:]...)

	tqa.recomputeTenantQueriers()
}

// notifyQuerierShutdown records that a querier has sent notification about a graceful shutdown.
func (tqa *tenantQuerierAssignments) notifyQuerierShutdown(querierID QuerierID) {
	querier := tqa.queriersByID[querierID]
	if querier == nil {
		// The querier may have already been removed, so we just ignore it.
		return
	}

	// If there are no more connections, we should remove the querier.
	if querier.connections == 0 {
		tqa.removeQuerier(querierID)
		return
	}

	// Otherwise we should annotate we received a graceful shutdown notification
	// and the querier will be removed once all connections are unregistered.
	querier.shuttingDown = true
}

// forgetDisconnectedQueriers removes all disconnected queriers that have gone since at least
// the forget delay. Returns the number of forgotten queriers.
func (tqa *tenantQuerierAssignments) forgetDisconnectedQueriers(now time.Time) int {
	// Nothing to do if the forget delay is disabled.
	if tqa.querierForgetDelay == 0 {
		return 0
	}

	// Remove all queriers with no connections that have gone since at least the forget delay.
	threshold := now.Add(-tqa.querierForgetDelay)
	forgotten := 0

	for querierID := range tqa.queriersByID {
		if querier := tqa.queriersByID[querierID]; querier.connections == 0 && querier.disconnectedAt.Before(threshold) {
			tqa.removeQuerier(querierID)
			forgotten++
		}
	}

	return forgotten
}

func (tqa *tenantQuerierAssignments) recomputeTenantQueriers() {
	var scratchpad querierIDSlice
	for tenantID, tenant := range tqa.tenantsByID {
		if tenant.maxQueriers > 0 && tenant.maxQueriers < len(tqa.querierIDsSorted) && scratchpad == nil {
			// shuffle sharding is enabled and the number of queriers exceeds tenant maxQueriers,
			// meaning tenant querier assignments need computed via shuffle sharding;
			// allocate the scratchpad the first time this case is hit and it will be reused after
			scratchpad = make(querierIDSlice, 0, len(tqa.querierIDsSorted))
		}

		tqa.shuffleTenantQueriers(tenantID, scratchpad)
	}
}

func (tqa *tenantQuerierAssignments) shuffleTenantQueriers(tenantID TenantID, scratchpad querierIDSlice) {
	tenant := tqa.tenantsByID[tenantID]
	if tenant == nil {
		return
	}

	if tenant.maxQueriers == 0 || len(tqa.querierIDsSorted) <= tenant.maxQueriers {
		// shuffle shard is either disabled or calculation is unnecessary
		tqa.tenantQuerierIDs[tenantID] = nil
		return
	}

	querierIDSet := make(map[QuerierID]struct{}, tenant.maxQueriers)
	rnd := rand.New(rand.NewSource(tenant.shuffleShardSeed))

	scratchpad = append(scratchpad[:0], tqa.querierIDsSorted...)

	last := len(scratchpad) - 1
	for i := 0; i < tenant.maxQueriers; i++ {
		r := rnd.Intn(last + 1)
		querierIDSet[scratchpad[r]] = struct{}{}
		// move selected item to the end, it won't be selected anymore.
		scratchpad[r], scratchpad[last] = scratchpad[last], scratchpad[r]
		last--
	}
	tqa.tenantQuerierIDs[tenantID] = querierIDSet
}
