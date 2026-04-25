package fec

import (
	"errors"

	"github.com/MeteorsLiu/multipath/internal/debuglog"
	"github.com/klauspost/reedsolomon"
)

var (
	ErrInvalidShardConfig = errors.New("fec: invalid shard config")
	ErrUnrecoverable      = errors.New("fec: unrecoverable shards")
)

const (
	tinyMT32Mat1 = uint32(0x8f7011ee)
	tinyMT32Mat2 = uint32(0xfc78ff1f)
	tinyMT32TMat = uint32(0x3793fdff)
	tinyMT32Mask = uint32(0x7fffffff)
)

type Codec struct {
	dataShards   int
	repairShards int
	low          reedsolomon.LowLevel
}

func NewCodec(dataShards, repairShards int) (*Codec, error) {
	if dataShards <= 0 || repairShards != 1 {
		debuglog.Printf("fec", "new_codec_err data_shards=%d repair_shards=%d err=%v", dataShards, repairShards, ErrInvalidShardConfig)
		return nil, ErrInvalidShardConfig
	}
	debuglog.Printf("fec", "new_codec data_shards=%d repair_shards=%d", dataShards, repairShards)
	return &Codec{
		dataShards:   dataShards,
		repairShards: repairShards,
	}, nil
}

func (c *Codec) Encode(shards [][]byte, key uint16) error {
	if debuglog.Enabled() {
		dataShards, repairShards := debugCodecShape(c)
		debuglog.Printf("fec", "encode_start key=%d data_shards=%d repair_shards=%d lens=%v", key, dataShards, repairShards, debugShardLens(shards))
	}
	if err := c.validate(shards); err != nil {
		debuglog.Printf("fec", "encode_validate_err key=%d err=%v", key, err)
		return err
	}

	repairLen := 0
	for i := 0; i < c.dataShards; i++ {
		if len(shards[i]) > repairLen {
			repairLen = len(shards[i])
		}
	}
	if repairLen == 0 {
		debuglog.Printf("fec", "encode_err key=%d err=%v reason=empty_repair", key, ErrInvalidShardConfig)
		return ErrInvalidShardConfig
	}

	for i := 0; i < c.dataShards; i++ {
		if shards[i] == nil {
			debuglog.Printf("fec", "encode_err key=%d shard=%d err=%v reason=nil_data_shard", key, i, ErrInvalidShardConfig)
			return ErrInvalidShardConfig
		}
	}

	repair := shards[c.dataShards]
	if cap(repair) < repairLen {
		repair = make([]byte, repairLen)
	} else {
		repair = repair[:repairLen]
	}
	clear(repair)

	var coeffBuf [32]byte
	var coeffs []byte
	if c.dataShards > len(coeffBuf) {
		coeffs = make([]byte, c.dataShards)
	} else {
		coeffs = coeffBuf[:c.dataShards]
	}
	fillCodingCoefficients(key, coeffs)
	for i := 0; i < c.dataShards; i++ {
		c.low.GalMulSliceXor(coeffs[i], shards[i], repair[:len(shards[i])])
	}

	shards[c.dataShards] = repair
	debuglog.Printf("fec", "encode_done key=%d repair_len=%d", key, len(repair))
	return nil
}

func (c *Codec) Reconstruct(shards [][]byte, key uint16) error {
	if debuglog.Enabled() {
		dataShards, repairShards := debugCodecShape(c)
		debuglog.Printf("fec", "reconstruct_start key=%d data_shards=%d repair_shards=%d lens=%v", key, dataShards, repairShards, debugShardLens(shards))
	}
	if err := c.validate(shards); err != nil {
		debuglog.Printf("fec", "reconstruct_validate_err key=%d err=%v", key, err)
		return err
	}

	repair := shards[c.dataShards]
	if len(repair) == 0 {
		debuglog.Printf("fec", "reconstruct_err key=%d err=%v reason=empty_repair", key, ErrUnrecoverable)
		return ErrUnrecoverable
	}
	repairLen := len(repair)

	missingIndex := -1
	missingCount := 0
	for i := 0; i < c.dataShards; i++ {
		if len(shards[i]) == 0 {
			missingIndex = i
			missingCount++
		}
	}
	if missingCount != 1 {
		debuglog.Printf("fec", "reconstruct_err key=%d missing_count=%d err=%v", key, missingCount, ErrUnrecoverable)
		return ErrUnrecoverable
	}

	for i := 0; i < c.dataShards; i++ {
		if i == missingIndex {
			continue
		}
		if len(shards[i]) > repairLen {
			debuglog.Printf("fec", "reconstruct_err key=%d shard=%d shard_len=%d repair_len=%d err=%v", key, i, len(shards[i]), repairLen, ErrUnrecoverable)
			return ErrUnrecoverable
		}
	}

	var coeffBuf [32]byte
	var coeffs []byte
	if c.dataShards > len(coeffBuf) {
		coeffs = make([]byte, c.dataShards)
	} else {
		coeffs = coeffBuf[:c.dataShards]
	}
	fillCodingCoefficients(key, coeffs)

	recovered := shards[missingIndex]
	if cap(recovered) < repairLen {
		recovered = make([]byte, repairLen)
	} else {
		recovered = recovered[:repairLen]
	}
	copy(recovered, repair)
	for i := 0; i < c.dataShards; i++ {
		if i == missingIndex {
			continue
		}
		c.low.GalMulSliceXor(coeffs[i], shards[i], recovered[:len(shards[i])])
	}
	c.low.GalMulSlice(reedsolomon.Inv(coeffs[missingIndex]), recovered, recovered)

	shards[missingIndex] = recovered
	debuglog.Printf("fec", "reconstruct_done key=%d missing_index=%d recovered_len=%d", key, missingIndex, len(recovered))
	return nil
}

func (c *Codec) validate(shards [][]byte) error {
	if c == nil || c.dataShards <= 0 || c.repairShards != 1 {
		return ErrInvalidShardConfig
	}
	if len(shards) != c.dataShards+c.repairShards {
		return ErrInvalidShardConfig
	}
	return nil
}

func fillCodingCoefficients(key uint16, coeffs []byte) {
	rng := newTinyMT32(uint32(key))
	for i := range coeffs {
		for coeffs[i] == 0 {
			coeffs[i] = byte(rng.generate() & 0xff)
		}
	}
}

func debugShardLens(shards [][]byte) []int {
	lens := make([]int, len(shards))
	for i := range shards {
		lens[i] = len(shards[i])
	}
	return lens
}

func debugCodecShape(c *Codec) (int, int) {
	if c == nil {
		return 0, 0
	}
	return c.dataShards, c.repairShards
}

type tinyMT32 struct {
	status [4]uint32
}

func newTinyMT32(seed uint32) tinyMT32 {
	var rng tinyMT32
	rng.status[0] = seed
	rng.status[1] = tinyMT32Mat1
	rng.status[2] = tinyMT32Mat2
	rng.status[3] = tinyMT32TMat

	for i := uint32(1); i < 8; i++ {
		prev := rng.status[(i-1)&3]
		rng.status[i&3] ^= i + 1812433253*(prev^(prev>>30))
	}
	for i := 0; i < 8; i++ {
		rng.nextState()
	}
	return rng
}

func (r *tinyMT32) generate() uint32 {
	r.nextState()
	return r.temper()
}

func (r *tinyMT32) nextState() {
	y := r.status[3]
	x := (r.status[0] & tinyMT32Mask) ^ r.status[1] ^ r.status[2]
	x ^= x << 1
	y ^= (y >> 1) ^ x
	r.status[0] = r.status[1]
	r.status[1] = r.status[2]
	r.status[2] = x ^ (y << 10)
	r.status[3] = y
	if y&1 != 0 {
		r.status[1] ^= tinyMT32Mat1
		r.status[2] ^= tinyMT32Mat2
	}
}

func (r *tinyMT32) temper() uint32 {
	t0 := r.status[3]
	t1 := r.status[0] + (r.status[2] >> 8)
	t0 ^= t1
	if t1&1 != 0 {
		t0 ^= tinyMT32TMat
	}
	return t0
}
