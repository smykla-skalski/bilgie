//go:build !linux && !windows

package osshim

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestDecodeLsofName(t *testing.T) {
	assert.Equal(t, "/p/Proj_\xc3\x84/Target/f", decodeLsofName(`/p/Proj_\xc3\x84/Target/f`))
	assert.Equal(t, "/p/a\nb", decodeLsofName(`/p/a\nb`))
	assert.Equal(t, "/p/a\x01b", decodeLsofName(`/p/a^Ab`))
	assert.Equal(t, `/p/a\xZZ`, decodeLsofName(`/p/a\xZZ`))
	assert.Equal(t, `/p/a^b`, decodeLsofName(`/p/a^b`))
}

func TestMarkOpenAncestorsMatchesWholeComponents(t *testing.T) {
	byDir := map[string][]string{
		lsofKey("/Users/X/Proj/target"):  {"/Users/X/Proj/target"},
		lsofKey("/Users/X/Proj/target2"): {"/Users/X/Proj/target2"},
	}
	open := map[string]bool{}
	markOpenAncestors(open, byDir, "/Users/X/Proj/target/a/f")
	assert.Equal(t, map[string]bool{"/Users/X/Proj/target": true}, open)

	markOpenAncestors(open, byDir, "/Users/X/Proj/target2")
	assert.True(t, open["/Users/X/Proj/target2"])
}
