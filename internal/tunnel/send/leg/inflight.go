package leg

type InflightKey struct {
	Target    uint64
	PingID    uint64
	SessionID uint64
	LaneID    uint8
	Leg       ProbeLegKey
}

type ProbeLegKey struct {
	Kind       uint8
	EndpointID string
	Remote     string
	ConnID     string
}

type inflightTracker struct {
	entries map[inflightMapKey]inflightPing
}

type inflightMapKey struct {
	target uint64
	pingID uint64
}

type inflightPing struct {
	sessionID  uint64
	laneID     uint8
	leg        ProbeLegKey
	timeMS     uint64
	deadlineMS uint64
}

// record registers an in-flight ping and returns the legs of pings that timed
// out waiting for a pong, so callers can count them as failed deliveries.
func (t *inflightTracker) record(key InflightKey, timeMS uint64, deadlineMS uint64, timeoutMS uint64) []ProbeLegKey {
	t.init()
	expired := t.pruneTarget(key.Target, timeMS, timeoutMS)
	t.entries[inflightMapKey{target: key.Target, pingID: key.PingID}] = inflightPing{
		sessionID:  key.SessionID,
		laneID:     key.LaneID,
		leg:        key.Leg,
		timeMS:     timeMS,
		deadlineMS: deadlineMS,
	}
	return expired
}

func (t *inflightTracker) accept(key InflightKey, timeMS uint64, nowMS uint64) (sampleMS uint32, deadlineMS uint64, ok bool) {
	mapKey := inflightMapKey{target: key.Target, pingID: key.PingID}
	ping, ok := t.entries[mapKey]
	if !ok ||
		ping.sessionID != key.SessionID ||
		ping.laneID != key.LaneID ||
		ping.leg != key.Leg ||
		ping.timeMS != timeMS ||
		nowMS < timeMS {
		return 0, 0, false
	}

	delete(t.entries, mapKey)
	sample64 := nowMS - timeMS
	sampleMS = uint32(sample64)
	if sample64 > uint64(^uint32(0)) {
		sampleMS = ^uint32(0)
	}
	return sampleMS, ping.deadlineMS, true
}

func (t *inflightTracker) clearTarget(target uint64) {
	for key := range t.entries {
		if key.target == target {
			delete(t.entries, key)
		}
	}
}

func (t *inflightTracker) len() int {
	return len(t.entries)
}

func (t *inflightTracker) pruneTarget(target uint64, nowMS uint64, timeoutMS uint64) []ProbeLegKey {
	if timeoutMS == 0 {
		return nil
	}
	var expired []ProbeLegKey
	for key, ping := range t.entries {
		if key.target != target || ping.timeMS > nowMS {
			continue
		}
		if nowMS-ping.timeMS >= timeoutMS {
			delete(t.entries, key)
			expired = append(expired, ping.leg)
		}
	}
	return expired
}

func (t *inflightTracker) init() {
	if t.entries == nil {
		t.entries = make(map[inflightMapKey]inflightPing)
	}
}
