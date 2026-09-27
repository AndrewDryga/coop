package workerproto

import (
	"strings"
	"testing"
)

func TestPublishRequestPinsCandidateAndDestination(t *testing.T) {
	valid := PublishRequest{
		AuthorizationRef: "approval:one", CandidateHead: strings.Repeat("a", 40),
		CandidateTree: strings.Repeat("b", 40), Branch: "coop/fix", BaseBranch: "main",
		Title: "Fix a bug", Body: "Reviewed changes.",
	}
	for _, length := range []int{40, 64} {
		for _, number := range []int{0, 7} {
			request := valid
			request.CandidateHead = strings.Repeat("a", length)
			request.CandidateTree = strings.Repeat("b", length)
			request.PullRequestNumber = number
			if number > 0 {
				request.ExpectedHead = strings.Repeat("c", length)
			}
			if err := request.Validate(); err != nil {
				t.Fatalf("valid %d-character identity, PR %d: %v", length, number, err)
			}
		}
	}
	for name, change := range map[string]func(*PublishRequest){
		"missing approval":        func(r *PublishRequest) { r.AuthorizationRef = "" },
		"oversized approval":      func(r *PublishRequest) { r.AuthorizationRef = strings.Repeat("a", 1025) },
		"abbreviated commit":      func(r *PublishRequest) { r.CandidateHead = "abcdef0" },
		"uppercase tree":          func(r *PublishRequest) { r.CandidateTree = strings.Repeat("A", 40) },
		"invalid expected head":   func(r *PublishRequest) { r.ExpectedHead = "main" },
		"existing PR without CAS": func(r *PublishRequest) { r.PullRequestNumber = 7 },
		"negative PR":             func(r *PublishRequest) { r.PullRequestNumber = -1 },
		"base overwrite":          func(r *PublishRequest) { r.Branch = r.BaseBranch },
		"empty title":             func(r *PublishRequest) { r.Title = " \n" },
		"oversized title":         func(r *PublishRequest) { r.Title = strings.Repeat("a", 257) },
		"oversized body":          func(r *PublishRequest) { r.Body = strings.Repeat("a", (32<<10)+1) },
		"NUL body":                func(r *PublishRequest) { r.Body = "hello\x00world" },
		"invalid UTF8":            func(r *PublishRequest) { r.Title = string([]byte{0xff}) },
	} {
		t.Run(name, func(t *testing.T) {
			request := valid
			change(&request)
			if request.Validate() == nil {
				t.Fatal("invalid publication accepted")
			}
		})
	}
	for _, branch := range []string{"", "-force", "main..fix", "main@{0}", "a//b", ".hidden", "a/b.lock", "a/", "a.", "a b", "a\nb", "a:other", strings.Repeat("a", 241)} {
		for _, base := range []bool{false, true} {
			request := valid
			if base {
				request.BaseBranch = branch
			} else {
				request.Branch = branch
			}
			if request.Validate() == nil {
				t.Fatalf("invalid branch %q accepted (base=%v)", branch, base)
			}
		}
	}
}
