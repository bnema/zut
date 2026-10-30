package modes

import (
	"github.com/bnema/zut/packages/provider"
)

func requestUserTextCount(req provider.Request, want string) int {
	count := 0
	for _, message := range req.Messages {
		if message.Role != provider.RoleUser {
			continue
		}
		for _, content := range message.Content {
			if text, ok := content.(provider.TextBlock); ok && text.Text == want {
				count++
			}
		}
	}
	return count
}
