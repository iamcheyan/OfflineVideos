package provider

// RSSProvider, LocalProvider and PodcastProvider are reserved provider names.
// Each must implement VideoProvider before it can be registered. No
// authentication, protocol behavior, or content support is implied here.
type RSSProvider struct{}
type LocalProvider struct{}
type PodcastProvider struct{}
