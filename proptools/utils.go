package proptools

import (
	"errors"
	"math"
	"strings"
)

func ShardBySize[T ~[]E, E any](toShard T, shardSize int) []T {
	if len(toShard) == 0 {
		return nil
	}

	ret := make([]T, 0, (len(toShard)+shardSize-1)/shardSize)
	for len(toShard) > shardSize {
		ret = append(ret, toShard[0:shardSize])
		toShard = toShard[shardSize:]
	}
	if len(toShard) > 0 {
		ret = append(ret, toShard)
	}
	return ret
}

func ShardByCount[T ~[]E, E any](toShard T, shardCount int) []T {
	return ShardBySize(toShard, int(math.Ceil(float64(len(toShard))/float64(shardCount))))
}

// MergeErrors merges a list of errors into a single error.
func MergeErrors(errs []error) error {
	if errs != nil {
		var sb strings.Builder
		for i, err := range errs {
			if i != 0 {
				sb.WriteString("\n")
			}
			sb.WriteString(err.Error())
		}
		return errors.New(sb.String())
	}
	return nil
}
