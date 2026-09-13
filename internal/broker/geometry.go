package broker

import (
	"fmt"
	"strconv"
)

type Geometry struct{ Cols, WindowRows, PTYRows int }

func ComputeGeometry(width, height int, status string) (Geometry, error) {
	if width < 1 || width > 1000 || height < 1 || height > 1000 {
		return Geometry{}, fmt.Errorf("tmux geometry out of bounds: %dx%d", width, height)
	}
	rows := 0
	switch status {
	case "off":
	case "on":
		rows = 1
	default:
		n, e := strconv.Atoi(status)
		if e != nil || n < 1 || n > 5 {
			return Geometry{}, fmt.Errorf("unexpected tmux status value %q", status)
		}
		rows = n
	}
	return Geometry{Cols: width, WindowRows: height, PTYRows: height + rows}, nil
}
