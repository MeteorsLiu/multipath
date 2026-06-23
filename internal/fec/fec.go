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
}

func NewCodec(dataShards, repairShards int) (*Codec, error) {
	if dataShards <= 0 || repairShards <= 0 || repairShards > 4 {
		debuglog.Printf("fec", "new_codec_err data_shards=%d repair_shards=%d err=%v", dataShards, repairShards, ErrInvalidShardConfig)
		return nil, ErrInvalidShardConfig
	}
	debuglog.Printf("fec", "new_codec data_shards=%d repair_shards=%d", dataShards, repairShards)
	return &Codec{dataShards: dataShards, repairShards: repairShards}, nil
}

func (c *Codec) Encode(shards [][]byte, keys []uint16) error {
	if debuglog.Enabled() {
		dataShards, repairShards := debugCodecShape(c)
		debuglog.Printf("fec", "encode_start keys=%v data_shards=%d repair_shards=%d lens=%v", keys, dataShards, repairShards, debugShardLens(shards))
	}
	if err := c.validate(shards, keys); err != nil {
		debuglog.Printf("fec", "encode_validate_err keys=%v err=%v", keys, err)
		return err
	}

	repairLen := maxShardLen(shards[:c.dataShards])
	if repairLen == 0 {
		debuglog.Printf("fec", "encode_err keys=%v err=%v reason=empty_repair", keys, ErrInvalidShardConfig)
		return ErrInvalidShardConfig
	}

	work := make([][]byte, c.dataShards+c.repairShards)
	for i := 0; i < c.dataShards; i++ {
		if shards[i] == nil {
			debuglog.Printf("fec", "encode_err keys=%v shard=%d err=%v reason=nil_data_shard", keys, i, ErrInvalidShardConfig)
			return ErrInvalidShardConfig
		}
		work[i] = paddedShard(shards[i], repairLen)
	}
	for i := 0; i < c.repairShards; i++ {
		repair := shards[c.dataShards+i]
		if cap(repair) < repairLen {
			repair = make([]byte, repairLen)
		} else {
			repair = repair[:repairLen]
			clear(repair)
		}
		work[c.dataShards+i] = repair
	}
	encoder, err := c.encoder(keys)
	if err != nil {
		return err
	}
	if err := encoder.Encode(work); err != nil {
		return err
	}
	for i := 0; i < c.repairShards; i++ {
		shards[c.dataShards+i] = work[c.dataShards+i]
	}
	if debuglog.Enabled() {
		debuglog.Printf("fec", "encode_done keys=%v repair_len=%d", keys, repairLen)
	}
	return nil
}

func (c *Codec) Reconstruct(shards [][]byte, keys []uint16) error {
	if debuglog.Enabled() {
		dataShards, repairShards := debugCodecShape(c)
		debuglog.Printf("fec", "reconstruct_start keys=%v data_shards=%d repair_shards=%d lens=%v", keys, dataShards, repairShards, debugShardLens(shards))
	}
	if err := c.validate(shards, keys); err != nil {
		debuglog.Printf("fec", "reconstruct_validate_err keys=%v err=%v", keys, err)
		return err
	}

	shardLen := maxShardLen(shards)
	if shardLen == 0 {
		return ErrUnrecoverable
	}
	missingData := 0
	work := make([][]byte, c.dataShards+c.repairShards)
	for i := 0; i < c.dataShards; i++ {
		if len(shards[i]) == 0 {
			missingData++
			continue
		}
		if len(shards[i]) > shardLen {
			return ErrUnrecoverable
		}
		work[i] = paddedShard(shards[i], shardLen)
	}
	for i := 0; i < c.repairShards; i++ {
		shard := shards[c.dataShards+i]
		if len(shard) == 0 {
			continue
		}
		if len(shard) != shardLen {
			return ErrUnrecoverable
		}
		work[c.dataShards+i] = shard
	}
	if missingData == 0 || missingData > c.repairShards {
		return ErrUnrecoverable
	}
	encoder, err := c.encoder(keys)
	if err != nil {
		return err
	}
	if err := encoder.ReconstructData(work); err != nil {
		debuglog.Printf("fec", "reconstruct_err keys=%v missing_count=%d err=%v", keys, missingData, err)
		return ErrUnrecoverable
	}
	for i := 0; i < c.dataShards; i++ {
		if len(shards[i]) == 0 {
			shards[i] = work[i]
		}
	}
	if debuglog.Enabled() {
		debuglog.Printf("fec", "reconstruct_done keys=%v recovered_len=%d", keys, shardLen)
	}
	return nil
}

func (c *Codec) validate(shards [][]byte, keys []uint16) error {
	if c == nil || c.dataShards <= 0 || c.repairShards <= 0 || c.repairShards > 4 {
		return ErrInvalidShardConfig
	}
	if len(shards) != c.dataShards+c.repairShards || len(keys) != c.repairShards {
		return ErrInvalidShardConfig
	}
	return nil
}

func (c *Codec) encoder(keys []uint16) (reedsolomon.Encoder, error) {
	matrix := make([][]byte, c.repairShards)
	for i, key := range keys {
		row := make([]byte, c.dataShards)
		fillCodingCoefficients(key, row)
		matrix[i] = row
	}
	return reedsolomon.New(
		c.dataShards,
		c.repairShards,
		reedsolomon.WithCustomMatrix(matrix),
		reedsolomon.WithMaxGoroutines(1),
	)
}

func maxShardLen(shards [][]byte) int {
	size := 0
	for _, shard := range shards {
		if len(shard) > size {
			size = len(shard)
		}
	}
	return size
}

func paddedShard(shard []byte, size int) []byte {
	if len(shard) == size {
		return shard
	}
	out := make([]byte, size)
	copy(out, shard)
	return out
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
