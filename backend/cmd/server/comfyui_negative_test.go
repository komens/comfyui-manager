package main

import "testing"

// 覆盖提交器负面优先级：本条自带非空→用它；自带空/纯空白→回落工作流静态值。
// 关键回归：绝不能把「本条为空」误当成「清空负面」，那会让老工作流丢掉自带负向。
func TestResolveEffectiveNegative(t *testing.T) {
	cases := []struct {
		name     string
		item     string
		workflow string
		want     string
	}{
		{"本条自带覆盖工作流", "lowres, bad hands", "watermark", "lowres, bad hands"},
		{"本条为空回落工作流", "", "watermark", "watermark"},
		{"本条纯空白回落工作流", "   \t ", "watermark", "watermark"},
		{"本条非空且工作流为空", "text", "", "text"},
		{"两者皆空", "", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := resolveEffectiveNegative(c.item, c.workflow); got != c.want {
				t.Errorf("resolveEffectiveNegative(%q, %q) = %q，期望 %q", c.item, c.workflow, got, c.want)
			}
		})
	}
}
