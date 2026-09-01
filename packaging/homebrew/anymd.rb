# anymd — Homebrew formula.
#
# This file is the TEMPLATE / reference copy. The formula Homebrew actually
# installs lives in a separate tap repository:
#
#     github.com/muthuishere/homebrew-tap  ->  Formula/anymd.rb
#
# Once the `brews:` block in .goreleaser.yaml is uncommented, GoReleaser
# generates and commits that file on every release, filling in the version and
# the four sha256 values from the real archives. Until that tap repo exists,
# a maintainer can copy this file there by hand and fill the SHAs from the
# release's checksums.txt.
#
# Install path for users:
#
#     brew install muthuishere/tap/anymd
#
class Anymd < Formula
  desc "Any document to Markdown, in pure Go — one static binary, 15 converters"
  homepage "https://github.com/muthuishere/anymd"
  version "0.1.0"
  license "MIT"

  # ---------------------------------------------------------------------
  # SHA256 PLACEHOLDERS — every "REPLACE_WITH_SHA256_..." below is filled in
  # automatically by GoReleaser, or by hand from the release's checksums.txt:
  #
  #   curl -sSfL https://github.com/muthuishere/anymd/releases/download/v0.1.0/checksums.txt
  #
  # A formula shipped with these strings still in place will fail to install,
  # which is the intended failure mode: loud, not silent.
  # ---------------------------------------------------------------------

  on_macos do
    if Hardware::CPU.arm?
      url "https://github.com/muthuishere/anymd/releases/download/v#{version}/anymd_#{version}_darwin_arm64.tar.gz"
      sha256 "REPLACE_WITH_SHA256_DARWIN_ARM64"
    else
      url "https://github.com/muthuishere/anymd/releases/download/v#{version}/anymd_#{version}_darwin_amd64.tar.gz"
      sha256 "REPLACE_WITH_SHA256_DARWIN_AMD64"
    end
  end

  on_linux do
    if Hardware::CPU.arm?
      url "https://github.com/muthuishere/anymd/releases/download/v#{version}/anymd_#{version}_linux_arm64.tar.gz"
      sha256 "REPLACE_WITH_SHA256_LINUX_ARM64"
    else
      url "https://github.com/muthuishere/anymd/releases/download/v#{version}/anymd_#{version}_linux_amd64.tar.gz"
      sha256 "REPLACE_WITH_SHA256_LINUX_AMD64"
    end
  end

  def install
    bin.install "anymd"
  end

  def caveats
    <<~EOS
      anymd converts documents to Markdown offline. It deliberately ships no
      OCR, no audio transcription and no LLM captioning, and it never touches
      the network while converting — only an explicit http(s) URL argument is
      fetched.

      List the converters available in this build:  anymd --list
    EOS
  end

  # The test asserts REAL CONVERSION, not that the binary can print its own
  # version. A binary that starts but mis-renders a table is still broken.
  test do
    # 1. version stamping actually happened (a "dev" build means the ldflags
    #    were lost somewhere in the release pipeline).
    assert_match version.to_s, shell_output("#{bin}/anymd --version")

    # 2. stdin -> a real GFM pipe table.
    out = pipe_output("#{bin}/anymd -t csv", "name,qty\nbolt,4\n")
    assert_match "| name | qty |", out
    assert_match "| --- | --- |", out
    assert_match "| bolt | 4 |", out

    # 3. a file argument goes to stdout, and JSON comes back fenced.
    (testpath/"d.json").write('{"k":[1,2]}')
    assert_match "```json", shell_output("#{bin}/anymd #{testpath}/d.json")

    # 4. the registry is non-empty and contains the flagship converters.
    listed = shell_output("#{bin}/anymd --list")
    %w[csv docx html json pdf pptx xlsx zip plaintext].each do |conv|
      assert_match conv, listed
    end

    # 5. the exit-code contract: 2 means "you called me wrong". shell_output's
    #    second argument asserts the expected exit status.
    shell_output("#{bin}/anymd --no-such-flag 2>/dev/null", 2)
  end
end
