package graph

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestNewGraph_AppMetadataMapsStayNil(t *testing.T) {
	g := NewGraph(nil, nil, time.Unix(1, 0).UTC())
	assert.Nil(t, g.NamespaceOwners)
	assert.Nil(t, g.ApplicationDeployers)
}
