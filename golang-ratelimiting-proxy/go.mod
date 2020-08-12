module github.com/honeycombio/exapmles/golang-ratelimiting-proxy

go 1.14

require (
	github.com/honeycombio/beeline-go v0.6.1
	github.com/honeycombio/leakybucket v0.0.0-20170302201951-7dee6281d8e9
)

// replace github.com/honeycombio/beeline-go v0.6.1 => github.com/maplebed/beeline-go v0.6.2-0.20200811233952-7d201795f2e4

replace github.com/honeycombio/beeline-go => /Users/ben/git/maplebed/beeline-go
