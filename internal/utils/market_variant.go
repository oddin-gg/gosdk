package utils

import "strings"

func IsMarketVariantWithDynamicOutcomes(marketVariant string) bool {
	return strings.HasPrefix(marketVariant, "od:dynamic_outcomes:")
}
