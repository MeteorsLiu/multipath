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
	if c.repairShards == 1 {
		return c.encodeSingle(shards, keys[0])
	}
	encoder, err := c.validatedEncoder(keys)
	if err != nil {
		debuglog.Printf("fec", "encode_keyset_err keys=%v err=%v", keys, err)
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
	if c.repairShards == 1 {
		return c.reconstructSingle(shards, keys[0])
	}
	encoder, err := c.validatedEncoder(keys)
	if err != nil {
		debuglog.Printf("fec", "reconstruct_keyset_err keys=%v err=%v", keys, err)
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
			work[i] = shards[i]
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

func (c *Codec) encodeSingle(shards [][]byte, key uint16) error {
	repairLen := maxShardLen(shards[:c.dataShards])
	if repairLen == 0 {
		debuglog.Printf("fec", "encode_err keys=[%d] err=%v reason=empty_repair", key, ErrInvalidShardConfig)
		return ErrInvalidShardConfig
	}

	for i := 0; i < c.dataShards; i++ {
		if shards[i] == nil {
			debuglog.Printf("fec", "encode_err keys=[%d] shard=%d err=%v reason=nil_data_shard", key, i, ErrInvalidShardConfig)
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
	if debuglog.Enabled() {
		debuglog.Printf("fec", "encode_done keys=[%d] repair_len=%d", key, len(repair))
	}
	return nil
}

func (c *Codec) reconstructSingle(shards [][]byte, key uint16) error {
	repair := shards[c.dataShards]
	if len(repair) == 0 {
		debuglog.Printf("fec", "reconstruct_err keys=[%d] err=%v reason=empty_repair", key, ErrUnrecoverable)
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
		debuglog.Printf("fec", "reconstruct_err keys=[%d] missing_count=%d err=%v", key, missingCount, ErrUnrecoverable)
		return ErrUnrecoverable
	}

	for i := 0; i < c.dataShards; i++ {
		if i == missingIndex {
			continue
		}
		if len(shards[i]) > repairLen {
			debuglog.Printf("fec", "reconstruct_err keys=[%d] shard=%d shard_len=%d repair_len=%d err=%v", key, i, len(shards[i]), repairLen, ErrUnrecoverable)
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
	if debuglog.Enabled() {
		debuglog.Printf("fec", "reconstruct_done keys=[%d] recovered_len=%d", key, len(recovered))
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

func (c *Codec) validatedEncoder(keys []uint16) (reedsolomon.Encoder, error) {
	encoder, err := c.encoder(keys)
	if err != nil {
		return nil, ErrInvalidShardConfig
	}
	if !c.canReconstructDataSubsets(encoder) {
		return nil, ErrInvalidShardConfig
	}
	return encoder, nil
}

func (c *Codec) canReconstructDataSubsets(encoder reedsolomon.Encoder) bool {
	maxMissing := c.repairShards
	if maxMissing > c.dataShards {
		maxMissing = c.dataShards
	}
	for missingCount := 1; missingCount <= maxMissing; missingCount++ {
		if !c.canReconstructDataSubsetsOfSize(encoder, missingCount, 0, make([]int, 0, missingCount)) {
			return false
		}
	}
	return true
}

func (c *Codec) canReconstructDataSubsetsOfSize(encoder reedsolomon.Encoder, target, start int, missing []int) bool {
	if len(missing) == target {
		return c.canReconstructDataSubset(encoder, missing)
	}
	remaining := target - len(missing)
	for i := start; i <= c.dataShards-remaining; i++ {
		missing = append(missing, i)
		if !c.canReconstructDataSubsetsOfSize(encoder, target, i+1, missing) {
			return false
		}
		missing = missing[:len(missing)-1]
	}
	return true
}

func (c *Codec) canReconstructDataSubset(encoder reedsolomon.Encoder, missing []int) bool {
	shards := make([][]byte, c.dataShards+c.repairShards)
	for i := 0; i < c.dataShards; i++ {
		shards[i] = []byte{byte(i + 1)}
	}
	for i := 0; i < c.repairShards; i++ {
		shards[c.dataShards+i] = make([]byte, 1)
	}
	if err := encoder.Encode(shards); err != nil {
		return false
	}
	for _, index := range missing {
		shards[index] = nil
	}
	if err := encoder.ReconstructData(shards); err != nil {
		return false
	}
	for _, index := range missing {
		if len(shards[index]) != 1 || shards[index][0] != byte(index+1) {
			return false
		}
	}
	return true
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
