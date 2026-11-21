This directory contains the textarea package from Bubbles v2.2.1. Petal keeps
a local copy because the wrapping function is private. The only change to
the upstream Go files is in textarea.go, where wrap tracks printable ASCII
words to avoid measuring each prefix. The module path lets the copy use the
same internal helpers as upstream.

When updating Bubbles, compare both Go files with the new textarea package.
Remove this module and its replace directive once upstream provides equivalent
performance. Run BenchmarkComposerEdit and TestComposerWrapMatchesBubbles in
internal/tui to check the update.
