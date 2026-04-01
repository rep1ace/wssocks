package main

import (
	"errors"
	"flag"
	"github.com/genshen/cmds"
	_ "github.com/rep1ace/wssocks/cmd/client"
	_ "github.com/rep1ace/wssocks/cmd/server"
	_ "github.com/rep1ace/wssocks/version"
	log "github.com/sirupsen/logrus"
)

func init() {
	log.SetLevel(log.TraceLevel)
}

func main() {
	cmds.SetProgramName("wssocks")
	if err := cmds.Parse(); err != nil {
		if !errors.Is(err, flag.ErrHelp) && !errors.Is(err, &cmds.SubCommandParseError{}) {
			log.Fatal(err)
		}
	}
}
