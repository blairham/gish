# Core-shaped Homebrew formula for koi (#164).
#
# Not submittable yet, and docs/packaging.md says why with the numbers:
# homebrew/homebrew-core wants 30 forks / 30 watchers / 75 stars for a
# third-party submission and triple that (225 stars) for a self-submission
# by the repository owner. Finding someone else to open the PR is worth
# 3x, so the submission is deliberately not ours to make. This file exists
# so that when the bar trips, submitting is a same-day action rather than
# a week of reconstruction.
#
# Differences from the tap formula GoReleaser generates:
#   - built from source, which is what core prefers for Go programs
#   - no caveats telling anyone to run chsh: "never require chsh" is our
#     own rule, and core reviewers dislike caveats regardless
#   - only the version is stamped. The tap build stamps commit and date
#     too, but core builds from a release tarball, which carries no git
#     metadata — stamping the tap owner into main.commit (as this file
#     used to) put the string "homebrew" where a commit hash belongs, and
#     a build timestamp would make the build non-reproducible. main.go
#     already defaults both to "none"/"unknown".
#   - the test actually runs the shell rather than checking --version,
#     because a binary that prints its version and cannot run a command
#     is exactly the breakage worth catching
#
# Replace VERSION and SHA on submission; the release workflow prints both.
class Koi < Formula
  desc "Interactive shell with bash compatibility and native plugins"
  homepage "https://github.com/blairham/koi-shell"
  url "https://github.com/blairham/koi-shell/archive/refs/tags/vVERSION.tar.gz"
  sha256 "SHA"
  license "MIT"
  head "https://github.com/blairham/koi-shell.git", branch: "main"

  depends_on "go" => :build

  def install
    ldflags = %W[
      -s -w
      -X main.version=#{version}
    ]
    system "go", "build", *std_go_args(ldflags: ldflags), "./cmd/koi"
    system "go", "build", *std_go_args(ldflags: ldflags, output: bin/"koi-git"), "./cmd/koi-git"
    system "go", "build", *std_go_args(ldflags: ldflags, output: bin/"koi-carapace"), "./cmd/koi-carapace"
  end

  test do
    # It runs a command, and it is POSIX-clean on the -c path.
    assert_equal "ok", shell_output("#{bin}/koi -c 'echo ok'").strip
    # Its own diagnostics agree that the install is sane. Verified to exit 0
    # under `env -i` with an empty HOME and no TERM — doctor exits non-zero
    # only on ✘, and a bare install produces ⚠ at worst.
    system bin/"koi", "-c", "doctor >/dev/null"
    assert_match "koi", shell_output("#{bin}/koi --version")
    # The bundled plugins got built and installed, rather than the install
    # step silently producing only the shell.
    assert_predicate bin/"koi-git", :executable?
    assert_predicate bin/"koi-carapace", :executable?
  end
end
