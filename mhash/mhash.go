package mhash

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"github.com/QuangTung97/memproxy"
	"github.com/QuangTung97/memproxy/item"
	"math"
)

// ErrHashTooDeep when too many levels to go to
var ErrHashTooDeep = errors.New("mhash: hash go too deep")

const maxDeepLevels = 5

// Null ...
type Null[T any] struct {
	Valid bool
	Data  T
}

const bitSetShift = 3
const bitSetMask = 1<<bitSetShift - 1
const bitSetBytes = 256 / (1 << bitSetShift)

// BitSet ...
type BitSet [bitSetBytes]byte

// Bucket ...
type Bucket[T item.Value] struct {
	Items  []T
	Bitset BitSet
}

// BucketKey ...
type BucketKey[R item.Key] struct {
	RootKey R
	Hash    uint64
	HashLen int
}

// String ...
func (k BucketKey[R]) String() string {
	var data [8]byte
	binary.BigEndian.PutUint64(data[:], k.Hash)
	return k.RootKey.String() + ":" + hex.EncodeToString(data[:k.HashLen])
}

// Filler ...
type Filler[T any, R any] func(ctx context.Context, rootKey R, hash uint64) func() ([]byte, error)

// Key types
type Key interface {
	comparable
	Hash() uint64
}

// Hash ...
type Hash[T item.Value, R item.Key, K Key] struct {
	sess   memproxy.Session
	getKey func(v T) K

	bucketItem *item.Item[Bucket[T], BucketKey[R]]
}

// HashUpdater ...
type HashUpdater[T item.Value, R item.Key, K Key] struct {
	sess        memproxy.Session
	getKey      func(v T) K
	unmarshaler item.Unmarshaler[Bucket[T]]
	filler      Filler[T, R]
}

// New ...
func New[T item.Value, R item.Key, K Key](
	sess memproxy.Session,
	pipeline memproxy.Pipeline,
	getKey func(v T) K,
	unmarshaler item.Unmarshaler[T],
	filler Filler[T, R],
) *Hash[T, R, K] {
	bucketUnmarshaler := BucketUnmarshalerFromItem(unmarshaler)

	var bucketFiller item.Filler[Bucket[T], BucketKey[R]] = func(
		ctx context.Context, key BucketKey[R],
	) func() (Bucket[T], error) {
		fn := filler(ctx, key.RootKey, key.Hash)
		return func() (Bucket[T], error) {
			data, err := fn()
			if err != nil {
				return Bucket[T]{}, err
			}
			return bucketUnmarshaler(data)
		}
	}

	return &Hash[T, R, K]{
		sess:   sess,
		getKey: getKey,

		bucketItem: item.New[Bucket[T], BucketKey[R]](
			sess, pipeline, bucketUnmarshaler, bucketFiller,
		),
	}
}

type getResult[T any] struct {
	resp Null[T]
	err  error
}

func computeHashAtLevel(hash uint64, hashLen int) uint64 {
	return hash & (math.MaxUint64 << (64 - 8*hashLen))
}

func computeBitOffsetAtNextLevel(hash uint64, currentHashLen int) int {
	offset := (hash >> (64 - 8 - currentHashLen*8)) & 0xff
	return int(offset)
}

// Get ...
func (h *Hash[T, R, K]) Get(ctx context.Context, rootKey R, key K) func() (Null[T], error) {
	keyHash := key.Hash()

	var rootBucketFn func() (Bucket[T], error)
	var nextCallFn func()
	hashLen := 0

	doGetFn := func() {
		rootBucketFn = h.bucketItem.Get(ctx, BucketKey[R]{
			RootKey: rootKey,
			Hash:    computeHashAtLevel(keyHash, hashLen),
			HashLen: hashLen,
		})
		h.sess.AddNextCall(nextCallFn)
	}

	var result getResult[T]
	nextCallFn = func() {
		bucket, err := rootBucketFn()
		if err != nil {
			result.err = err
			return
		}

		bitOffset := computeBitOffsetAtNextLevel(keyHash, hashLen)
		if bucket.Bitset.GetBit(bitOffset) {
			hashLen++
			if hashLen >= maxDeepLevels {
				result.err = ErrHashTooDeep
				return
			}
			doGetFn()
			return
		}

		for _, bucketItem := range bucket.Items {
			itemKey := h.getKey(bucketItem)
			if itemKey == key {
				result.resp = Null[T]{
					Valid: true,
					Data:  bucketItem,
				}
				return
			}
		}
	}

	doGetFn()

	return func() (Null[T], error) {
		h.sess.Execute()
		return result.resp, result.err
	}
}
