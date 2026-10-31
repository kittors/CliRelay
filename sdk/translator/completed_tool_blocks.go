package translator

// CompletedToolBlocksHeader is the request header an OpenAI-compatible client
// sends to opt in to completed tool-block markers. The executor acknowledges
// the negotiated version with the same response header.
const CompletedToolBlocksHeader = "X-Tool-Completion"

// CompletedToolBlocksVersion is the only accepted negotiation value.
const CompletedToolBlocksVersion = "block-stop-v1"
