package service

import (
	"time"

	"brainbreak-lab/internal/models"
)

func CalculateAge(birthDate, now time.Time, tz *time.Location) int {
	birthInTZ := birthDate.In(tz)
	nowInTZ := now.In(tz)
	years := nowInTZ.Year() - birthInTZ.Year()
	if nowInTZ.Month() < birthInTZ.Month() || (nowInTZ.Month() == birthInTZ.Month() && nowInTZ.Day() < birthInTZ.Day()) {
		years--
	}
	if years < 0 {
		years = 0
	}
	return years
}

func ClassifyAgeGroup(age int) models.AgeGroup {
	switch {
	case age >= 18:
		return models.AgeGroupAdult
	case age >= 13:
		return models.AgeGroupTeen
	default:
		return models.AgeGroupChild
	}
}

func AgeGroupFromBirthDate(birthDate, now time.Time, tz *time.Location) models.AgeGroup {
	age := CalculateAge(birthDate, now, tz)
	return ClassifyAgeGroup(age)
}
