package daemon

import (
	"strings"
)

func ParseMap(s string) map[string]string {
	pairs := strings.Split(s, ",")
	m := make(map[string]string, len(pairs))
	for _, pair := range pairs {
		split := strings.Split(pair, ":")
		if len(split) == 2 {
			m[strings.TrimSpace(split[0])] = strings.TrimSpace(split[1])
		}
	}
	return m
}
