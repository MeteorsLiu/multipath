package prober

type maxTimeHeap []*gcPacket

func (h maxTimeHeap) Len() int           { return len(h) }
func (h maxTimeHeap) Less(i, j int) bool { return h[i].elasped > h[j].elasped }
func (h maxTimeHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }

func (h *maxTimeHeap) Push(x any) {
	// Push and Pop use pointer receivers because they modify the slice's length,
	// not just its contents.
	*h = append(*h, x.(*gcPacket))
}

func (h *maxTimeHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[0 : n-1]
	return x
}
