package main

// truncateText 截断字符串到 maxRunes 个 rune，超出加 "..." 后缀。
func truncateText(value string, maxRunes int) string {
	runes := []rune(value)
	if len(runes) <= maxRunes {
		return value
	}
	return string(runes[:maxRunes]) + "..."
}

// appendRolling 追加一个元素并保留最后 max 个（滚动窗口）。
func appendRolling[T any](items []T, value T, max int) []T {
	items = append(items, value)
	if len(items) > max {
		items = append([]T(nil), items[len(items)-max:]...)
	}
	return items
}

// mergeUnique 将 values 中不在 dst 的非空字符串追加到 dst，返回去重后的切片。
func mergeUnique(dst []string, values ...string) []string {
	for _, value := range values {
		if value == "" {
			continue
		}
		found := false
		for _, existing := range dst {
			if existing == value {
				found = true
				break
			}
		}
		if !found {
			dst = append(dst, value)
		}
	}
	return dst
}
