package util

import "sort"

// NaturalLess сравнивает строки с учётом числовых блоков:
// "sa2" < "sa10", "key_1" < "key_2" < "key_10".
func NaturalLess(a, b string) bool {
	for a != "" || b != "" {
		if a == "" {
			return true
		}
		if b == "" {
			return false
		}
		ai := a[0]
		bi := b[0]
		da := ai >= '0' && ai <= '9'
		db := bi >= '0' && bi <= '9'
		switch {
		case da && !db:
			return true
		case !da && db:
			return false
		case da && db:
			na, restA := ReadNumber(a)
			nb, restB := ReadNumber(b)
			if na != nb {
				return na < nb
			}
			a, b = restA, restB
		default:
			if ai != bi {
				return ai < bi
			}
			a = a[1:]
			b = b[1:]
		}
	}
	return false
}

func ReadNumber(s string) (int, string) {
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	n := 0
	for _, c := range s[:i] {
		n = n*10 + int(c-'0')
	}
	return n, s[i:]
}

// SortStringsNatural сортирует строки с natural sort.
func SortStringsNatural(s []string) {
	sort.Slice(s, func(i, j int) bool {
		return NaturalLess(s[i], s[j])
	})
}
