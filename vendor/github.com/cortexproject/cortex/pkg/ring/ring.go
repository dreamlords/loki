package ring

// Based on https://raw.githubusercontent.com/stathat/consistent/master/consistent.go

import (
	"context"
	"crypto/md5"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"math"
	"math/rand"
	"sync"
	"time"

	"github.com/go-kit/kit/log/level"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/cortexproject/cortex/pkg/ring/kv"
	"github.com/cortexproject/cortex/pkg/util"
	"github.com/cortexproject/cortex/pkg/util/services"
)

const (
	unhealthy = "Unhealthy"

	// IngesterRingKey is the key under which we store the ingesters ring in the KVStore.
	IngesterRingKey = "ring"

	// RulerRingKey is the key under which we store the rulers ring in the KVStore.
	RulerRingKey = "ring"

	// DistributorRingKey is the key under which we store the distributors ring in the KVStore.
	DistributorRingKey = "distributor"

	// CompactorRingKey is the key under which we store the compactors ring in the KVStore.
	CompactorRingKey = "compactor"
)

// ReadRing represents the read interface to the ring.
type ReadRing interface {
	prometheus.Collector

	// Get returns n (or more) ingesters which form the replicas for the given key.
	// buf is a slice to be overwritten for the return value
	// to avoid memory allocation; can be nil.
	Get(key uint32, op Operation, buf []IngesterDesc) (ReplicationSet, error)
	GetAll(op Operation) (ReplicationSet, error)
	ReplicationFactor() int
	IngesterCount() int

	// ShuffleShard returns a subring for the provided identifier (eg. a tenant ID)
	// and size (number of instances).
	ShuffleShard(identifier string, size int) ReadRing

	// HasInstance returns whether the ring contains an instance matching the provided instanceID.
	HasInstance(instanceID string) bool
}

// Operation can be Read or Write
type Operation int

// Values for Operation
const (
	Read Operation = iota
	Write
	Reporting // Special value for inquiring about health

	// BlocksSync is the operation run by the store-gateway to sync blocks.
	BlocksSync

	// BlocksRead is the operation run by the querier to query blocks via the store-gateway.
	BlocksRead
)

var (
	// ErrEmptyRing is the error returned when trying to get an element when nothing has been added to hash.
	ErrEmptyRing = errors.New("empty ring")

	// ErrInstanceNotFound is the error returned when trying to get information for an instance
	// not registered within the ring.
	ErrInstanceNotFound = errors.New("instance not found in the ring")
)

// Config for a Ring
type Config struct {
	KVStore              kv.Config     `yaml:"kvstore"`
	HeartbeatTimeout     time.Duration `yaml:"heartbeat_timeout"`
	ReplicationFactor    int           `yaml:"replication_factor"`
	ZoneAwarenessEnabled bool          `yaml:"zone_awareness_enabled"`
}

// RegisterFlags adds the flags required to config this to the given FlagSet with a specified prefix
func (cfg *Config) RegisterFlags(f *flag.FlagSet) {
	cfg.RegisterFlagsWithPrefix("", f)
}

// RegisterFlagsWithPrefix adds the flags required to config this to the given FlagSet with a specified prefix
func (cfg *Config) RegisterFlagsWithPrefix(prefix string, f *flag.FlagSet) {
	cfg.KVStore.RegisterFlagsWithPrefix(prefix, "collectors/", f)

	f.DurationVar(&cfg.HeartbeatTimeout, prefix+"ring.heartbeat-timeout", time.Minute, "The heartbeat timeout after which ingesters are skipped for reads/writes.")
	f.IntVar(&cfg.ReplicationFactor, prefix+"distributor.replication-factor", 3, "The number of ingesters to write to and read from.")
	f.BoolVar(&cfg.ZoneAwarenessEnabled, prefix+"distributor.zone-awareness-enabled", false, "True to enable the zone-awareness and replicate ingested samples across different availability zones.")
}

// Ring holds the information about the members of the consistent hash ring.
type Ring struct {
	services.Service

	key      string
	cfg      Config
	KVClient kv.Client
	strategy ReplicationStrategy

	mtx              sync.RWMutex
	ringDesc         *Desc
	ringTokens       []TokenDesc
	ringTokensByZone map[string][]TokenDesc

	// List of zones for which there's at least 1 instance in the ring. This list is guaranteed
	// to be sorted alphabetically.
	ringZones []string

	memberOwnershipDesc *prometheus.Desc
	numMembersDesc      *prometheus.Desc
	totalTokensDesc     *prometheus.Desc
	numTokensDesc       *prometheus.Desc
	oldestTimestampDesc *prometheus.Desc
}

// New creates a new Ring. Being a service, Ring needs to be started to do anything.
func New(cfg Config, name, key string, reg prometheus.Registerer) (*Ring, error) {
	codec := GetCodec()
	// Suffix all client names with "-ring" to denote this kv client is used by the ring
	store, err := kv.NewClient(
		cfg.KVStore,
		codec,
		kv.RegistererWithKVName(reg, name+"-ring"),
	)
	if err != nil {
		return nil, err
	}

	return NewWithStoreClientAndStrategy(cfg, name, key, store, &DefaultReplicationStrategy{})
}

func NewWithStoreClientAndStrategy(cfg Config, name, key string, store kv.Client, strategy ReplicationStrategy) (*Ring, error) {
	if cfg.ReplicationFactor <= 0 {
		return nil, fmt.Errorf("ReplicationFactor must be greater than zero: %d", cfg.ReplicationFactor)
	}

	r := &Ring{
		key:      key,
		cfg:      cfg,
		KVClient: store,
		strategy: strategy,
		ringDesc: &Desc{},
		memberOwnershipDesc: prometheus.NewDesc(
			"cortex_ring_member_ownership_percent",
			"The percent ownership of the ring by member",
			[]string{"member"},
			map[string]string{"name": name},
		),
		numMembersDesc: prometheus.NewDesc(
			"cortex_ring_members",
			"Number of members in the ring",
			[]string{"state"},
			map[string]string{"name": name},
		),
		totalTokensDesc: prometheus.NewDesc(
			"cortex_ring_tokens_total",
			"Number of tokens in the ring",
			nil,
			map[string]string{"name": name},
		),
		numTokensDesc: prometheus.NewDesc(
			"cortex_ring_tokens_owned",
			"The number of tokens in the ring owned by the member",
			[]string{"member"},
			map[string]string{"name": name},
		),
		oldestTimestampDesc: prometheus.NewDesc(
			"cortex_ring_oldest_member_timestamp",
			"Timestamp of the oldest member in the ring.",
			[]string{"state"},
			map[string]string{"name": name},
		),
	}

	r.Service = services.NewBasicService(nil, r.loop, nil)
	return r, nil
}

func (r *Ring) loop(ctx context.Context) error {
	r.KVClient.WatchKey(ctx, r.key, func(value interface{}) bool {
		if value == nil {
			level.Info(util.Logger).Log("msg", "ring doesn't exist in consul yet")
			return true
		}

		ringDesc := value.(*Desc)
		ringTokens := ringDesc.getTokens()
		ringTokensByZone := ringDesc.getTokensByZone()
		ringZones := getZones(ringTokensByZone)

		r.mtx.Lock()
		defer r.mtx.Unlock()
		r.ringDesc = ringDesc
		r.ringTokens = ringTokens
		r.ringTokensByZone = ringTokensByZone
		r.ringZones = ringZones
		return true
	})
	return nil
}

// Get returns n (or more) ingesters which form the replicas for the given key.
func (r *Ring) Get(key uint32, op Operation, buf []IngesterDesc) (ReplicationSet, error) {
	r.mtx.RLock()
	defer r.mtx.RUnlock()
	if r.ringDesc == nil || len(r.ringTokens) == 0 {
		return ReplicationSet{}, ErrEmptyRing
	}

	var (
		n             = r.cfg.ReplicationFactor
		ingesters     = buf[:0]
		distinctHosts = map[string]struct{}{}
		distinctZones = map[string]struct{}{}
		start         = searchToken(r.ringTokens, key)
		iterations    = 0
	)
	for i := start; len(distinctHosts) < n && iterations < len(r.ringTokens); i++ {
		iterations++
		// Wrap i around in the ring.
		i %= len(r.ringTokens)

		// We want n *distinct* ingesters && distinct zones.
		token := r.ringTokens[i]
		if _, ok := distinctHosts[token.Ingester]; ok {
			continue
		}

		// Ignore if the ingesters don't have a zone set.
		if r.cfg.ZoneAwarenessEnabled && token.Zone != "" {
			if _, ok := distinctZones[token.Zone]; ok {
				continue
			}
			distinctZones[token.Zone] = struct{}{}
		}

		distinctHosts[token.Ingester] = struct{}{}
		ingester := r.ringDesc.Ingesters[token.Ingester]

		// Check whether the replica set should be extended given we're including
		// this instance.
		if r.strategy.ShouldExtendReplicaSet(ingester, op) {
			n++
		}

		ingesters = append(ingesters, ingester)
	}

	liveIngesters, maxFailure, err := r.strategy.Filter(ingesters, op, r.cfg.ReplicationFactor, r.cfg.HeartbeatTimeout, r.cfg.ZoneAwarenessEnabled)
	if err != nil {
		return ReplicationSet{}, err
	}

	return ReplicationSet{
		Ingesters: liveIngesters,
		MaxErrors: maxFailure,
	}, nil
}

// GetAll returns all available ingesters in the ring.
func (r *Ring) GetAll(op Operation) (ReplicationSet, error) {
	r.mtx.RLock()
	defer r.mtx.RUnlock()

	if r.ringDesc == nil || len(r.ringTokens) == 0 {
		return ReplicationSet{}, ErrEmptyRing
	}

	// Calculate the number of required ingesters;
	// ensure we always require at least RF-1 when RF=3.
	numRequired := len(r.ringDesc.Ingesters)
	if numRequired < r.cfg.ReplicationFactor {
		numRequired = r.cfg.ReplicationFactor
	}
	maxUnavailable := r.cfg.ReplicationFactor / 2
	numRequired -= maxUnavailable

	ingesters := make([]IngesterDesc, 0, len(r.ringDesc.Ingesters))
	for _, ingester := range r.ringDesc.Ingesters {
		if r.IsHealthy(&ingester, op) {
			ingesters = append(ingesters, ingester)
		}
	}

	if len(ingesters) < numRequired {
		return ReplicationSet{}, fmt.Errorf("too many failed ingesters")
	}

	return ReplicationSet{
		Ingesters: ingesters,
		MaxErrors: len(ingesters) - numRequired,
	}, nil
}

// Describe implements prometheus.Collector.
func (r *Ring) Describe(ch chan<- *prometheus.Desc) {
	ch <- r.memberOwnershipDesc
	ch <- r.numMembersDesc
	ch <- r.totalTokensDesc
	ch <- r.oldestTimestampDesc
	ch <- r.numTokensDesc
}

func countTokens(ringDesc *Desc, tokens []TokenDesc) (map[string]uint32, map[string]uint32) {
	owned := map[string]uint32{}
	numTokens := map[string]uint32{}
	for i, token := range tokens {
		var diff uint32
		if i+1 == len(tokens) {
			diff = (math.MaxUint32 - token.Token) + tokens[0].Token
		} else {
			diff = tokens[i+1].Token - token.Token
		}
		numTokens[token.Ingester] = numTokens[token.Ingester] + 1
		owned[token.Ingester] = owned[token.Ingester] + diff
	}

	for id := range ringDesc.Ingesters {
		if _, ok := owned[id]; !ok {
			owned[id] = 0
			numTokens[id] = 0
		}
	}

	return numTokens, owned
}

// Collect implements prometheus.Collector.
func (r *Ring) Collect(ch chan<- prometheus.Metric) {
	r.mtx.RLock()
	defer r.mtx.RUnlock()

	numTokens, ownedRange := countTokens(r.ringDesc, r.ringTokens)
	for id, totalOwned := range ownedRange {
		ch <- prometheus.MustNewConstMetric(
			r.memberOwnershipDesc,
			prometheus.GaugeValue,
			float64(totalOwned)/float64(math.MaxUint32),
			id,
		)
		ch <- prometheus.MustNewConstMetric(
			r.numTokensDesc,
			prometheus.GaugeValue,
			float64(numTokens[id]),
			id,
		)
	}

	numByState := map[string]int{}
	oldestTimestampByState := map[string]int64{}

	// Initialised to zero so we emit zero-metrics (instead of not emitting anything)
	for _, s := range []string{unhealthy, ACTIVE.String(), LEAVING.String(), PENDING.String(), JOINING.String()} {
		numByState[s] = 0
		oldestTimestampByState[s] = 0
	}

	for _, ingester := range r.ringDesc.Ingesters {
		s := ingester.State.String()
		if !r.IsHealthy(&ingester, Reporting) {
			s = unhealthy
		}
		numByState[s]++
		if oldestTimestampByState[s] == 0 || ingester.Timestamp < oldestTimestampByState[s] {
			oldestTimestampByState[s] = ingester.Timestamp
		}
	}

	for state, count := range numByState {
		ch <- prometheus.MustNewConstMetric(
			r.numMembersDesc,
			prometheus.GaugeValue,
			float64(count),
			state,
		)
	}
	for state, timestamp := range oldestTimestampByState {
		ch <- prometheus.MustNewConstMetric(
			r.oldestTimestampDesc,
			prometheus.GaugeValue,
			float64(timestamp),
			state,
		)
	}

	ch <- prometheus.MustNewConstMetric(
		r.totalTokensDesc,
		prometheus.GaugeValue,
		float64(len(r.ringTokens)),
	)
}

// ShuffleShard returns a subring for the provided identifier (eg. a tenant ID)
// and size (number of instances). The size is expected to be a multiple of the
// number of zones and the returned subring will contain the same number of
// instances per zone as far as there are enough registered instances in the ring.
//
// The algorithm used to build the subring is a shuffle sharder based on probabilistic
// hashing. We treat each zone as a separate ring and pick N unique replicas from each
// zone, walking the ring starting from random but predictable numbers. The random
// generator is initialised with a seed based on the provided identifier.
//
// This implementation guarantees:
//
// - Stability: given the same ring, two invocations returns the same result.
//
// - Consistency: adding/removing 1 instance from the ring generates a resulting
// subring with no more then 1 difference.
//
// - Shuffling: probabilistically, for a large enough cluster each identifier gets a different
// set of instances, with a reduced number of overlapping instances between two identifiers.
func (r *Ring) ShuffleShard(identifier string, size int) ReadRing {
	// Nothing to do if the shard size is not smaller then the actual ring.
	if size <= 0 || r.IngesterCount() <= size {
		return r
	}

	// Use the identifier to compute an hash we'll use to seed the random.
	hasher := md5.New()
	hasher.Write([]byte(identifier)) // nolint:errcheck
	checksum := hasher.Sum(nil)

	// Generate the seed based on the first 64 bits of the checksum.
	seed := int64(binary.BigEndian.Uint64(checksum))

	// Initialise the random generator used to select instances in the ring.
	random := rand.New(rand.NewSource(seed))

	r.mtx.RLock()
	defer r.mtx.RUnlock()

	var numInstancesPerZone int
	var actualZones []string

	if r.cfg.ZoneAwarenessEnabled {
		// When zone-awareness is enabled, we expect the shard size to be divisible
		// by the number of zones, in order to have nodes balanced across zones.
		// If it's not, we do round up.
		numInstancesPerZone = int(math.Ceil(float64(size) / float64(len(r.ringZones))))
		actualZones = r.ringZones
	} else {
		numInstancesPerZone = size
		actualZones = []string{""}
	}

	shard := make(map[string]IngesterDesc, size)

	// We need to iterate zones always in the same order to guarantee stability.
	for _, zone := range actualZones {
		var tokens []TokenDesc

		if r.cfg.ZoneAwarenessEnabled {
			tokens = r.ringTokensByZone[zone]
		} else {
			// When zone-awareness is disabled, we just iterate over 1 single fake zone
			// and use all tokens in the ring.
			tokens = r.ringTokens
		}

		// To select one more instance while guaranteeing the "consistency" property,
		// we do pick a random value from the generator and resolve uniqueness collisions
		// (if any) continuing walking the ring.
		for i := 0; i < numInstancesPerZone; i++ {
			start := searchToken(tokens, random.Uint32())
			iterations := 0
			found := false

			for p := start; iterations < len(tokens); p++ {
				iterations++

				// Wrap p around in the ring.
				p %= len(tokens)

				// Ensure we select an unique instance.
				if _, ok := shard[tokens[p].Ingester]; ok {
					continue
				}

				shard[tokens[p].Ingester] = r.ringDesc.Ingesters[tokens[p].Ingester]
				found = true
				break
			}

			// If one more instance has not been found, we can stop looking for
			// more instances in this zone, because it means the zone has no more
			// instances which haven't been already selected.
			if !found {
				break
			}
		}
	}

	// Build a read-only ring for the shard.
	shardDesc := &Desc{Ingesters: shard}
	shardTokensByZone := shardDesc.getTokensByZone()

	return &Ring{
		cfg:              r.cfg,
		strategy:         r.strategy,
		ringDesc:         shardDesc,
		ringTokens:       shardDesc.getTokens(),
		ringTokensByZone: shardTokensByZone,
		ringZones:        getZones(shardTokensByZone),
	}
}

// GetInstanceState returns the current state of an instance or an error if the
// instance does not exist in the ring.
func (r *Ring) GetInstanceState(instanceID string) (IngesterState, error) {
	r.mtx.RLock()
	defer r.mtx.RUnlock()

	instances := r.ringDesc.GetIngesters()
	instance, ok := instances[instanceID]
	if !ok {
		return PENDING, ErrInstanceNotFound
	}

	return instance.GetState(), nil
}

// HasInstance returns whether the ring contains an instance matching the provided instanceID.
func (r *Ring) HasInstance(instanceID string) bool {
	r.mtx.RLock()
	defer r.mtx.RUnlock()

	instances := r.ringDesc.GetIngesters()
	_, ok := instances[instanceID]
	return ok
}
