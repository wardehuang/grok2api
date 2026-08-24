// Package emailmatch 提供与 slots 接口一致的多键邮箱匹配语义。
// emailMatchKeys / gmailCanonicalEmail 的唯一实现，slots 与账号状态同步共用。
package emailmatch

import "strings"

// MatchKeys 返回一个地址的全部等价查找键：小写字面量 + Gmail 规范键。
// Gmail/Googlemail 本地部分忽略点和 +tag，变体折叠为同一规范键；
// 其他域名只有小写后的字面量一键。
func MatchKeys(rawEmail string) []string {
	normalizedEmail := strings.ToLower(strings.TrimSpace(rawEmail))
	if normalizedEmail == "" {
		return nil
	}
	keys := []string{normalizedEmail}
	if canonicalKey := gmailCanonicalEmail(normalizedEmail); canonicalKey != "" && canonicalKey != normalizedEmail {
		keys = append(keys, canonicalKey)
	}
	return keys
}

func gmailCanonicalEmail(normalizedEmail string) string {
	atIndex := strings.LastIndex(normalizedEmail, "@")
	if atIndex <= 0 || atIndex == len(normalizedEmail)-1 {
		return ""
	}
	localPart := normalizedEmail[:atIndex]
	domain := normalizedEmail[atIndex+1:]
	switch domain {
	case "gmail.com", "googlemail.com":
	default:
		return ""
	}
	if plusIndex := strings.IndexByte(localPart, '+'); plusIndex >= 0 {
		localPart = localPart[:plusIndex]
	}
	localPart = strings.ReplaceAll(localPart, ".", "")
	if localPart == "" {
		return ""
	}
	return localPart + "@gmail.com"
}
