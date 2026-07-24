package clock

import (
	"errors"
	"time"

	"brainbreak-lab/focus/internal/model"
)

// AgeBoundaries 定义年龄分组阈值（含下不含上）。
const (
	ChildMaxAge = 13 // [0,13) 儿童
	TeenMaxAge  = 18 // [13,18) 青少年; [18,∞) 成人
)

// LoadLocation 包装 time.LoadLocation 并校验，错误为通用校验错误避免泄漏内部信息。
func LoadLocation(name string) (*time.Location, error) {
	if name == "" {
		return nil, errors.New("timezone required")
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, errors.New("invalid timezone")
	}
	return loc, nil
}

// AgeAt 计算给定时刻（视为 subject 时区）下 subject 的周岁年龄。
// dateOfBirth 以 UTC 午夜存储，表示 subject 本地日历上的出生日期。
func AgeAt(dateOfBirth time.Time, at time.Time, loc *time.Location) int {
	if loc == nil {
		loc = time.UTC
	}
	dob := dateOfBirth.In(time.UTC)
	local := at.In(loc)
	y, m, d := local.Date()
	// 构造该本地日期的 UTC 午夜用于比较。
	birthY, birthM, birthD := dob.Date()
	age := y - birthY
	if m < birthM || (m == birthM && d < birthD) {
		age--
	}
	if age < 0 {
		age = 0
	}
	return age
}

// AgeGroupFor 返回周岁年龄对应的分组。
func AgeGroupFor(age int) model.AgeGroup {
	switch {
	case age < ChildMaxAge:
		return model.AgeChild
	case age < TeenMaxAge:
		return model.AgeTeen
	default:
		return model.AgeAdult
	}
}

// AgeGroupAt 便捷方法：计算某时刻分组。
func AgeGroupAt(dob time.Time, at time.Time, loc *time.Location) model.AgeGroup {
	return AgeGroupFor(AgeAt(dob, at, loc))
}

// LocalDate 把某时刻折算到给定时区的本地日期（返回 UTC 午夜，仅日期部分有意义）。
func LocalDate(at time.Time, loc *time.Location) time.Time {
	if loc == nil {
		loc = time.UTC
	}
	y, m, d := at.In(loc).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// ParseBedtime 解析 "HH:MM" 为当天的 time.Time（仅时钟字段有意义）。
func ParseBedtime(s string) (time.Time, error) {
	t, err := time.Parse("15:04", s)
	if err != nil {
		return time.Time{}, errors.New("invalid bedtime")
	}
	return t, nil
}

// BedtimeWindow 计算 at 所在本地自然日的睡前禁刷窗口 [bedtime-1h, bedtime)。
// 若窗口上界早于下界（如 bedtime=00:30 时 start 落在前一日），调用方按本地日期解释即可。
func BedtimeWindow(bedtime time.Time, at time.Time, loc *time.Location) (start, end time.Time) {
	if loc == nil {
		loc = time.UTC
	}
	y, m, d := at.In(loc).Date()
	end = time.Date(y, m, d, bedtime.Hour(), bedtime.Minute(), 0, 0, loc)
	start = end.Add(-1 * time.Hour)
	return start, end
}

// Overlaps 报告区间 [aStart,aEnd) 与 [bStart,bEnd) 是否相交。
func Overlaps(aStart, aEnd, bStart, bEnd time.Time) bool {
	if aEnd.Before(aStart) {
		aStart, aEnd = aEnd, aStart
	}
	if bEnd.Before(bStart) {
		bStart, bEnd = bEnd, bStart
	}
	return aStart.Before(bEnd) && bStart.Before(aEnd)
}
