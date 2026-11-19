package linkedin

import "context"

// Tests do not wait between requests.
func init() { pause = func(context.Context) {} }
