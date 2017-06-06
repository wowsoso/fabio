// Package file implements a simple file based registry
// backend which reads the routes from a file once.
package file

import (
	"io/ioutil"
	"log"

	"github.com/wowsoso/fabioz/registry"
	"github.com/wowsoso/fabioz/registry/static"
)

func NewBackend(filename string) (registry.Backend, error) {
	data, err := ioutil.ReadFile(filename)
	if err != nil {
		log.Println("[ERROR] Cannot read routes from ", filename)
		return nil, err
	}
	return static.NewBackend(string(data))
}
