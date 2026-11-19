// Package all registers every integration. Import it for its side effects.
package all

import (
	_ "github.com/fulldump/datame-suitcase/plugins/bluesky"
	_ "github.com/fulldump/datame-suitcase/plugins/folder"
	_ "github.com/fulldump/datame-suitcase/plugins/github"
	_ "github.com/fulldump/datame-suitcase/plugins/mastodon"
	_ "github.com/fulldump/datame-suitcase/plugins/rss"
	_ "github.com/fulldump/datame-suitcase/plugins/telegram"
	_ "github.com/fulldump/datame-suitcase/plugins/twitter"
)
