package tests_test

import (
	"context"
	"testing"
	"time"

	"github.com/simonepri/prometheus-api-bridge/tests"
)

func TestStatic(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if err := tests.Static(ctx, tests.ChartDir()); err != nil {
		t.Fatal(err)
	}
}
