// Package memo is a plugin's memory of its source, shared by every plugin
// that keeps one: the cache file in state_dir (File), the walks that refresh
// it (Flights), the changes it announces to Watch streams and the background
// work those streams' scopes ask for (Changes), and the lifetime both run
// under (Life). docs/plugin-standard.md rules 7, 8, 9, 13, 14 and 15 are what
// it implements; the README says how a plugin adopts it.
package memo
