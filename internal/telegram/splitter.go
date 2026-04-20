package telegram

import (
	"strings"
	"unicode/utf8"
)

const TelegramTextLimit = 4096

func SplitMessage(text string, limit int) []string {
	if limit <= 0 {
		limit = TelegramTextLimit
	}
	if utf8.RuneCountInString(text) <= limit {
		return []string{text}
	}

	out := make([]string, 0, 2)
	for utf8.RuneCountInString(text) > limit {
		idx := safeCut(text, limit)
		head, tail := text[:idx], text[idx:]
		out = append(out, strings.TrimRight(head, "\n "))
		text = strings.TrimLeft(tail, "\n ")
	}
	if text != "" {
		out = append(out, text)
	}
	return out
}

func safeCut(text string, limit int) int {
	maxByte := byteIndexAtRune(text, limit)
	if maxByte == len(text) {
		return maxByte
	}

	minAcceptable := maxByte / 2

	if i := lastIndexBefore(text, maxByte, "\n\n"); i >= minAcceptable {
		return safeAdjust(text, i+2, maxByte)
	}
	if i := strings.LastIndexByte(text[:maxByte], '\n'); i >= minAcceptable {
		return safeAdjust(text, i+1, maxByte)
	}
	if i := strings.LastIndexByte(text[:maxByte], ' '); i >= minAcceptable {
		return safeAdjust(text, i+1, maxByte)
	}

	return safeAdjust(text, maxByte, maxByte)
}

func safeAdjust(text string, cut, maxByte int) int {
	if cut > maxByte {
		cut = maxByte
	}
	if cut <= 0 {
		cut = maxByte
	}
	return avoidTagSplit(text, cut)
}

func avoidTagSplit(text string, cut int) int {
	prefix := text[:cut]
	open := strings.LastIndexByte(prefix, '<')
	if open < 0 {
		return cut
	}
	close := strings.LastIndexByte(prefix, '>')
	if close > open {
		return cut
	}

	if open == 0 {
		return cut
	}
	return open
}

func byteIndexAtRune(text string, runeCount int) int {
	if runeCount <= 0 {
		return 0
	}
	count := 0
	for i := range text {
		if count == runeCount {
			return i
		}
		count++
	}
	return len(text)
}

func lastIndexBefore(text string, maxByte int, sub string) int {
	if maxByte > len(text) {
		maxByte = len(text)
	}
	return strings.LastIndex(text[:maxByte], sub)
}
