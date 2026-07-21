package main

import (
	_ "github.com/mattn/go-sqlite3"
	"maunium.net/go/mautrix/bridgev2/matrix/mxmain"

	"github.com/Leicas/matrisms/pkg/connector"
)

// Information to find out exactly which commit the bridge was built from.
// These are filled at build time with the -X linker flag.
var (
	Tag       = "unknown"
	Commit    = "unknown"
	BuildTime = "unknown"
)

var c = &connector.SMSConnector{}
var m = mxmain.BridgeMain{
	Name:        "matrisms",
	URL:         "https://github.com/Leicas/matrisms",
	Description: "Matrisms — bidirectional Matrix↔SMS/MMS bridge for VoIP.ms.",
	Version:     "0.1.0",
	Connector:   c,
}

func main() {
	m.InitVersion(Tag, Commit, BuildTime)
	m.Run()
}
