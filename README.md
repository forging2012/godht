# godht

A very simple DHT crawler in Golang.

## Install
```bash
go get github.com/fanpei91/godht
```

## Usage
```go
package main

import (
	"fmt"

	"github.com/fanpei91/godht"
)

func main() {
	laddr, maxFriendsPerSec := "0.0.0.0:6881", 400
	dht, err := godht.New(laddr, godht.MaxFriendsPerSec(maxFriendsPerSec))
	if err != nil {
		panic(err)
	}
	for announce := range dht.Announce {
		fmt.Println(fmt.Sprintf("link: magnet:?xt=urn:btih:%v\nnode: %s\npeer: %s\n",
			announce.InfohashHex,
			announce.From.String(),
			announce.Peer.String(),
		))
	}
}
```

## API
#### func  Bootstraps

```go
func Bootstraps(addr []string) option
```

#### func  LocalID

```go
func LocalID(id []byte) option
```

#### func  MaxFriendsPerSec

```go
func MaxFriendsPerSec(n int) option
```

#### func  Secret

```go
func Secret(s string) option
```

#### type Announce

```go
type Announce struct {
	Raw         map[string]interface{}
	From        *net.UDPAddr
	Peer        *net.TCPAddr
	Infohash    []byte
	InfohashHex string
}
```


#### type GoDHT

```go
type GoDHT struct {
	Announce chan *Announce
}
```


#### func  New

```go
func New(laddr string, options ...option) (*GoDHT, error)
```
