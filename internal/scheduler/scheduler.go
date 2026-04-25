package scheduler

type Scheduler interface {
	Enqueue(laneID uint8, weight uint32, charge uint32) error
	Dequeue() (laneID uint8, ok bool)
}
