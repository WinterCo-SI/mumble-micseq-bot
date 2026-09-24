package micseq

import (
	"strings"

	"golang.org/x/text/collate"
	"golang.org/x/text/language"
)

// NameLess returns a comparison for display names. With pinyin set, Chinese
// names sort by pinyin and Latin names case-insensitively; otherwise names
// sort case-insensitively by code point. The returned function is not safe
// for concurrent use.
func NameLess(pinyin bool) func(a, b string) bool {
	if !pinyin {
		return func(a, b string) bool {
			la, lb := strings.ToLower(a), strings.ToLower(b)
			if la != lb {
				return la < lb
			}
			return a < b
		}
	}
	c := collate.New(language.Chinese, collate.IgnoreCase, collate.Numeric)
	return func(a, b string) bool {
		if r := c.CompareString(a, b); r != 0 {
			return r < 0
		}
		return a < b
	}
}
