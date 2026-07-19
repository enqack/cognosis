package query

import "sort"

// Generic reciprocal-rank fusion. Structural, not behavioral -- the element
// type is a type parameter, keyed by a caller-supplied identity function, so
// the same merge fuses ranked chunks today and any future ranked type without
// an interface or a runtime type assertion.

// Leg is one ranked candidate list with its fusion weight. Rank is implicit:
// position in Items, 1-based.
type Leg[T any] struct {
	Items  []T
	Weight float64
}

// Scored is a fused item with its combined score.
type Scored[T any] struct {
	Item  T
	Score float64
}

// FuseRRF merges legs by summing weight/(k+rank) per distinct key, keeping
// the first-seen item for each key and ordering by descending score. Ties
// break by first appearance, keeping the order deterministic.
func FuseRRF[T any, K comparable](k int, key func(T) K, legs []Leg[T]) []Scored[T] {
	type slot struct {
		item  T
		score float64
		seen  int // arrival order for deterministic ties
	}
	slots := map[K]*slot{}
	order := 0
	for _, leg := range legs {
		for i, item := range leg.Items {
			rank := i + 1
			id := key(item)
			s, ok := slots[id]
			if !ok {
				s = &slot{item: item, seen: order}
				order++
				slots[id] = s
			}
			s.score += leg.Weight / float64(k+rank)
		}
	}
	out := make([]Scored[T], 0, len(slots))
	for _, s := range slots {
		out = append(out, Scored[T]{Item: s.item, Score: s.score})
	}
	seen := func(sc Scored[T]) int { return slots[key(sc.Item)].seen }
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return seen(out[i]) < seen(out[j])
	})
	return out
}
