# gocat

gocat concatenates Go source files within a package

## usage
```
gocat auth*.go > auth.go
```

Using grep and xargs to filter out tests can be done like so:

    ls *.go | grep -v _test | xargs gocat > p2p/aio.go

## license

See LICENSE file.

Copyright (c) 2014 The Go Authors. All rights reserved.

Originally authored by Russ Cox as [bundle](https://code.google.com/p/rsc/source/browse/cmd/bundle/main.go)

Modified by Joseph Naegele, 2015.

Modified by David Vennik, 2022.
