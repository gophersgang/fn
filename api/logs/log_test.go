package logs

import (
	logTesting "github.com/fnproject/fn/api/logs/testing"
	"testing"
)

func TestDatastore(t *testing.T) {
	ls := NewMock()
	logTesting.Test(t, ls)
}
