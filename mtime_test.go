package erofs

import "testing"

func TestMostCommonMtime(t *testing.T) {
	type mtime struct {
		sec  uint64
		nsec uint32
	}
	for _, tc := range []struct {
		name   string
		mtimes []mtime
		want   uint64
	}{
		{"empty", nil, 0},
		{"most frequent", []mtime{{100, 0}, {200, 0}, {100, 0}, {300, 0}}, 100},
		{"tie picks smallest", []mtime{{200, 0}, {100, 0}, {200, 0}, {100, 0}}, 100},
		{"ignores fractional", []mtime{{200, 1}, {200, 1}, {200, 1}, {300, 0}}, 300},
		{"only fractional", []mtime{{200, 1}, {300, 5}}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newMostCommonMtime()
			for _, m := range tc.mtimes {
				p.observe(m.sec, m.nsec)
			}
			if sec, nsec := p.result(); sec != tc.want || nsec != 0 {
				t.Errorf("result() = (%d, %d), want (%d, 0)", sec, nsec, tc.want)
			}
		})
	}
}
