package noms

import (
	"github.com/liquidata-inc/ld/dolt/go/libraries/doltcore/table"
	"github.com/liquidata-inc/ld/dolt/go/store/types"
)

// NomsMapWriteCloser is a TableWriteCloser where the resulting map that is being written from can be retrieved after
// it is closed.
type NomsMapWriteCloser interface {
	table.TableWriteCloser

	// GetMap retrieves the resulting types.Map once close is called
	GetMap() *types.Map
}
