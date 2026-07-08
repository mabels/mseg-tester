package configsync

import "testing"

func TestOwnerRepoFromURL(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{in: "https://github.com/mabels/mseg-tester-config", want: "mabels/mseg-tester-config"},
		{in: "https://github.com/mabels/mseg-tester-config/", want: "mabels/mseg-tester-config"},
		{in: "https://github.com/mabels/mseg-tester-config.git", want: "mabels/mseg-tester-config"},
		{in: "http://github.com/mabels/mseg-tester-config", want: "mabels/mseg-tester-config"},
		{in: "git@github.com:mabels/mseg-tester-config.git", want: "mabels/mseg-tester-config"},
		{in: "mabels/mseg-tester-config", want: "mabels/mseg-tester-config"},
		{in: "  https://github.com/mabels/mseg-tester-config  ", want: "mabels/mseg-tester-config"},
		{in: "https://github.com/", wantErr: true},
		{in: "not-a-repo-at-all", wantErr: true},
		{in: "", wantErr: true},
	}
	for _, c := range cases {
		got, err := ownerRepoFromURL(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("ownerRepoFromURL(%q) = %q, want error", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ownerRepoFromURL(%q) unexpected error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ownerRepoFromURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
