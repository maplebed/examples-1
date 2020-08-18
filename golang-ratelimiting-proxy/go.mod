module github.com/honeycombio/exapmles/golang-ratelimiting-proxy

go 1.14

require (
	github.com/davecgh/go-spew v1.1.1
	github.com/honeycombio/beeline-go v0.6.1
	github.com/honeycombio/leakybucket v0.0.0-20170302201951-7dee6281d8e9
)

// replace github.com/honeycombio/beeline-go v0.6.1 => github.com/maplebed/beeline-go v0.6.2-0.20200812185818-84a012014728

replace github.com/honeycombio/beeline-go => /Users/ben/git/maplebed/beeline-go
