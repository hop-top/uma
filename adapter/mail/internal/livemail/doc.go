// Package livemail is the test support of the opt-in live mail tests: it
// reads the endpoints of the containers started by testdata/live/up.sh (or
// any server answering the same protocols) from the environment, and wraps
// the three things the tests ask of them: an IMAP account to set up and
// inspect, a catcher to read submitted mail back from, and a sending MTA.
//
// Only tests import it. A test calls Require first: without UMA_LIVE_MAIL=1
// it skips, so the default test run and make check never need Docker or a
// mail server.
package livemail
