package vary

import "math"

type Vary struct {
	n, avg, pow, vary float64
}

func NewVary() *Vary {
	return &Vary{}
}

func (v *Vary) IsZero() bool {
	return v.n == 0 && v.avg == 0 && v.pow == 0 && v.vary == 0
}

func (v *Vary) Round() float64 {
	return v.n
}

func (v *Vary) Reset() {
	v.n = 0
	v.avg = 0
	v.pow = 0
	v.vary = 0
}

func (v *Vary) Calculate(x float64) float64 {
	v.n++
	v.avg = v.avg*(v.n-1)/v.n + x/v.n
	v.pow += math.Pow(x, 2)
	// 当前标准差
	v.vary = math.Sqrt((v.pow / v.n) - math.Pow(v.avg, 2))
	return v.vary
}

func (v *Vary) UCL(k float64) float64 {
	return v.avg + k*v.vary
}

func (v *Vary) LCL(k float64) float64 {
	return v.avg - k*v.vary
}

func (v *Vary) Avg() float64 {
	return v.avg
}
