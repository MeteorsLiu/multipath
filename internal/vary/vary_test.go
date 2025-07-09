package vary

import "testing"

func TestVary(t *testing.T) {
	n := []float64{5, 6, 16, 9}

	v := NewVary()

	var last float64
	for _, nn := range n {
		last = v.Calculate(nn)
	}

	if last != 4.301162633521313 {
		t.Error("misbehave")
	}
}

func TestVaryGroup(t *testing.T) {
	n := []float64{5, 6, 16, 9}

	v := NewVaryGroup()

	for _, nn := range n {
		v.Calculate(nn)
	}
	avg := v.Avg()
	v.Reset(func(v *Vary) float64 {
		return v.Avg()
	})

	for _, nn := range n {
		v.Calculate(nn)
	}

	if v.GroupAvg() != avg {
		t.Error("misbehave", v.Avg(), avg)
	}

}
