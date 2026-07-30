package schedule

type Lane interface {
	comparable
	Weight() uint32
	Cost(uint32) uint32
}

type Strategy[L Lane] interface {
	Pick(lanes []L, cost uint32) (L, bool)
}
