package bytesize

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode"

	"gopkg.in/yaml.v3"
)

type Size int64

func (s *Size) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode {
		return fmt.Errorf("invalid byte size node kind %v", node.Kind)
	}

	value, err := Parse(node.Value)
	if err != nil {
		return fmt.Errorf("invalid byte size %q: %w", node.Value, err)
	}
	*s = Size(value)
	return nil
}

func Parse(input string) (int64, error) {
	s := strings.TrimSpace(input)
	if s == "" {
		return 0, errors.New("value is empty")
	}

	if value, err := strconv.ParseInt(s, 10, 64); err == nil {
		if value < 0 {
			return 0, errors.New("value must be non-negative")
		}
		return value, nil
	}

	numberEnd := strings.IndexFunc(s, func(r rune) bool {
		return !(unicode.IsDigit(r) || r == '.')
	})
	if numberEnd <= 0 {
		return 0, errors.New("missing numeric value")
	}

	numberPart := strings.TrimSpace(s[:numberEnd])
	unitPart := strings.ToLower(strings.TrimSpace(s[numberEnd:]))
	if numberPart == "" || unitPart == "" {
		return 0, errors.New("size must include a number and unit")
	}

	numberValue, err := strconv.ParseFloat(numberPart, 64)
	if err != nil {
		return 0, fmt.Errorf("parse numeric value: %w", err)
	}
	if numberValue < 0 {
		return 0, errors.New("value must be non-negative")
	}

	multiplier, ok := byteSizeMultipliers[unitPart]
	if !ok {
		return 0, fmt.Errorf("unsupported unit %q", unitPart)
	}

	if !strings.Contains(numberPart, ".") {
		numberValue, err := strconv.ParseInt(numberPart, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("parse numeric value: %w", err)
		}
		if numberValue < 0 {
			return 0, errors.New("value must be non-negative")
		}
		if multiplier != 0 && numberValue > math.MaxInt64/multiplier {
			return 0, errors.New("size overflows int64")
		}
		return numberValue * multiplier, nil
	}

	value := numberValue * float64(multiplier)
	rounded := math.Round(value)
	if math.Abs(value-rounded) > 1e-9 {
		return 0, errors.New("size resolves to fractional bytes")
	}
	if rounded > math.MaxInt64 {
		return 0, errors.New("size overflows int64")
	}
	return int64(rounded), nil
}

var byteSizeMultipliers = map[string]int64{
	"b":   1,
	"k":   1 << 10,
	"kb":  1000,
	"kib": 1 << 10,
	"m":   1 << 20,
	"mb":  1000 * 1000,
	"mib": 1 << 20,
	"g":   1 << 30,
	"gb":  1000 * 1000 * 1000,
	"gib": 1 << 30,
	"t":   1 << 40,
	"tb":  1000 * 1000 * 1000 * 1000,
	"tib": 1 << 40,
	"p":   1 << 50,
	"pb":  1000 * 1000 * 1000 * 1000 * 1000,
	"pib": 1 << 50,
}
