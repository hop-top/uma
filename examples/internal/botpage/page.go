// Package botpage holds the chat page both bot examples serve.
package botpage

import _ "embed"

// Page is the chat page: the browser side of the widget channel. Both bot
// examples serve it unchanged at "/".
//
//go:embed index.html
var Page []byte
