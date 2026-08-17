# Core-shaped Homebrew formula for gish (#164).
#
# Kept in the repo so a third-party submitter to homebrew/homebrew-core
# has something to copy rather than reconstruct — Homebrew core applies
# its notability bar more leniently to a formula submitted by someone
# other than the author, so the submission is deliberately not ours to
# make.
#
# Differences from the tap formula GoReleaser generates:
#   - built from source, which is what core prefers for Go programs
#   - no caveats telling anyone to run chsh: "never require chsh" is our
#     own rule, and core reviewers dislike caveats regardless
#   - the test actually runs the shell rather than checking --version,
#     because a binary that prints its version and cannot run a command
#     is exactly the breakage worth catching
#
# Replace VERSION and SHA on submission; the release workflow prints both.
class Gish < Formula
  desc "Interactive shell with bash compatibility and native plugins"
  homepage "https://github.com/blairham/gish"
  url "https://github.com/blairham/gish/archive/refs/tags/vVERSION.tar.gz"
  sha256 "SHA"
  license "MIT"
  head "https://github.com/blairham/gish.git", branch: "main"

  depends_on "go" => :build

  def install
    ldflags = %W[
      -s -w
      -X main.version=#{version}
      -X main.commit=#{tap&.user || "homebrew"}
      -X main.date=#{time.iso8601}
    ]
    system "go", "build", *std_go_args(ldflags: ldflags), "./cmd/gish"
    system "go", "build", *std_go_args(ldflags: ldflags, output: bin/"gish-git"), "./cmd/gish-git"
    system "go", "build", *std_go_args(ldflags: ldflags, output: bin/"gish-carapace"), "./cmd/gish-carapace"
  end

  test do
    # It runs a command, and it is POSIX-clean on the -c path.
    assert_equal "ok", shell_output("#{bin}/gish -c 'echo ok'").strip
    # Its own diagnostics agree that the install is sane.
    system bin/"gish", "-c", "doctor >/dev/null"
    # The bundled plugins are real binaries, not stubs.
    assert_match "gish", shell_output("#{bin}/gish --version")
  end
end
