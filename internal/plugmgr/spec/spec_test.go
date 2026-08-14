package spec

import "testing"

func TestParsePlugin(t *testing.T) {
	cases := []struct {
		raw, from, idAs        string
		wantURL, wantID, wantU string
	}{
		{
			"zsh-users/zsh-autosuggestions", "", "",
			"https://github.com/zsh-users/zsh-autosuggestions", "zsh-users---zsh-autosuggestions", "zsh-users",
		},
		{
			"user/repo", "gl", "",
			"https://gitlab.com/user/repo", "user---repo", "user",
		},
		{
			"https://github.com/junegunn/fzf.git", "", "",
			"https://github.com/junegunn/fzf.git", "junegunn---fzf", "junegunn",
		},
		{
			"zsh-users/zsh-completions", "", "my-completions",
			"https://github.com/zsh-users/zsh-completions", "my-completions", "zsh-users",
		},
	}
	for _, tc := range cases {
		s, err := ParsePlugin(tc.raw, tc.from, tc.idAs)
		if err != nil {
			t.Fatalf("ParsePlugin(%q): %v", tc.raw, err)
		}
		if s.URL != tc.wantURL || s.ID != tc.wantID || s.User != tc.wantU {
			t.Errorf("ParsePlugin(%q) = url %q id %q user %q, want %q %q %q",
				tc.raw, s.URL, s.ID, s.User, tc.wantURL, tc.wantID, tc.wantU)
		}
	}
}

func TestParsePluginRejectsBadSpecs(t *testing.T) {
	for _, raw := range []string{"a/b/c", "/repo", "user/"} {
		if _, err := ParsePlugin(raw, "", ""); err == nil {
			t.Errorf("ParsePlugin(%q): want error, got nil", raw)
		}
	}
	if _, err := ParsePlugin("user/repo", "svn", ""); err == nil {
		t.Error("ParsePlugin with unknown from: want error, got nil")
	}
}

func TestParseSnippetAliases(t *testing.T) {
	cases := []struct{ raw, wantURL string }{
		{"OMZP::git", "https://raw.githubusercontent.com/ohmyzsh/ohmyzsh/master/plugins/git/git.plugin.zsh"},
		{"OMZP::kubectl/kubectl.plugin.zsh", "https://raw.githubusercontent.com/ohmyzsh/ohmyzsh/master/plugins/kubectl/kubectl.plugin.zsh"},
		{"OMZL::history.zsh", "https://raw.githubusercontent.com/ohmyzsh/ohmyzsh/master/lib/history.zsh"},
		{"OMZT::robbyrussell.zsh-theme", "https://raw.githubusercontent.com/ohmyzsh/ohmyzsh/master/themes/robbyrussell.zsh-theme"},
		{"PZTM::environment/init.zsh", "https://raw.githubusercontent.com/sorin-ionescu/prezto/master/modules/environment/init.zsh"},
		{"https://example.com/x.zsh", "https://example.com/x.zsh"},
	}
	for _, tc := range cases {
		s, err := ParseSnippet(tc.raw, "")
		if err != nil {
			t.Fatalf("ParseSnippet(%q): %v", tc.raw, err)
		}
		if s.URL != tc.wantURL {
			t.Errorf("ParseSnippet(%q).URL = %q, want %q", tc.raw, s.URL, tc.wantURL)
		}
	}
	if _, err := ParseSnippet("not-a-url", ""); err == nil {
		t.Error("ParseSnippet(not-a-url): want error, got nil")
	}
}
