package vary

type VaryGroup struct {
	current   Vary
	groupData Vary
}

func NewVaryGroup() *VaryGroup {
	return &VaryGroup{}
}

func (v *VaryGroup) Reset(f func(*Vary) float64) {
	v.groupData.Calculate(f(&v.current))
	v.current.Reset()
}

func (v *VaryGroup) Calculate(x float64) float64 {
	return v.current.Calculate(x)
}

func (v *VaryGroup) UCL(k float64) float64 {
	return v.current.UCL(k)
}

func (v *VaryGroup) LCL(k float64) float64 {
	return v.current.LCL(k)
}
func (v *VaryGroup) Avg() float64 {
	return v.current.Avg()
}

func (v *VaryGroup) GroupUCL(k float64) float64 {
	if v.groupData.Round() == 0 {
		return v.UCL(k)
	}
	return (v.current.UCL(k) + v.groupData.Avg()) / 2
}

func (v *VaryGroup) GroupLCL(k float64) float64 {
	if v.groupData.Round() == 0 {
		return v.LCL(k)
	}
	return (v.current.LCL(k) + v.groupData.Avg()) / 2
}
func (v *VaryGroup) GroupAvg() float64 {
	if v.groupData.Round() == 0 {
		return v.Avg()
	}
	return (v.current.Avg() + v.groupData.Avg()) / 2
}
