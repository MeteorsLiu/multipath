package cfs

import (
	"math"

	"github.com/MeteorsLiu/multipath/internal/schedule"
)

const defaultScale = uint64(1024)

type Strategy[L schedule.Lane] struct {
	items map[L]*item[L]
	minVR uint64
	round uint64
}

func New[L schedule.Lane]() *Strategy[L] {
	return &Strategy[L]{
		items: make(map[L]*item[L]),
	}
}

func (s *Strategy[L]) Pick(lanes []L, cost uint32) (L, bool) {
	var zero L
	if len(lanes) == 0 {
		return zero, false
	}
	if s.items == nil {
		s.items = make(map[L]*item[L])
	}
	s.round++

	var picked *item[L]
	for _, lane := range lanes {
		weight := lane.Weight()
		if weight == 0 {
			continue
		}
		it := s.item(lane)
		if it.lastSeen != 0 && it.lastSeen+1 < s.round && it.vruntime < s.minVR {
			it.vruntime = s.minVR
		}
		it.lastSeen = s.round
		if picked == nil || it.vruntime < picked.vruntime {
			picked = it
		}
	}
	if picked == nil {
		return zero, false
	}

	s.addCost(picked, cost, picked.lane.Weight())
	s.minVR = s.minCandidateVR(lanes)
	return picked.lane, true
}

func (s *Strategy[L]) item(lane L) *item[L] {
	it := s.items[lane]
	if it == nil {
		it = &item[L]{lane: lane, vruntime: s.minVR}
		s.items[lane] = it
	}
	return it
}

func (s *Strategy[L]) minCandidateVR(lanes []L) uint64 {
	minSet := false
	var min uint64
	for _, lane := range lanes {
		it := s.items[lane]
		if it == nil || lane.Weight() == 0 {
			continue
		}
		if !minSet || it.vruntime < min {
			min = it.vruntime
			minSet = true
		}
	}
	if !minSet {
		return s.minVR
	}
	return min
}

func (s *Strategy[L]) addCost(it *item[L], cost uint32, weight uint32) {
	if cost == 0 || weight == 0 {
		return
	}

	delta := uint64(cost) * defaultScale / uint64(weight)
	if math.MaxUint64-it.vruntime < delta {
		s.rebase(s.minVR)
	}
	it.vruntime += delta
}

func (s *Strategy[L]) rebase(base uint64) {
	if base == 0 {
		return
	}
	for _, it := range s.items {
		if it.vruntime >= base {
			it.vruntime -= base
		} else {
			it.vruntime = 0
		}
	}
	if s.minVR >= base {
		s.minVR -= base
	} else {
		s.minVR = 0
	}
}

type item[L schedule.Lane] struct {
	lane     L
	vruntime uint64
	lastSeen uint64
}
