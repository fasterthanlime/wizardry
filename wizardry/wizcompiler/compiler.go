package wizcompiler

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	humanize "github.com/dustin/go-humanize"
	"github.com/fasterthanlime/wizardry/wizardry/wizparser"
	"github.com/go-errors/errors"
)

type indentCallback func()

type ruleNode struct {
	id       int64
	rule     wizparser.Rule
	children []*ruleNode
}

type nodeEmitter func(node *ruleNode, defaultMarker string, prevSibling *ruleNode)

// Compile generates go code from a spellbook
func Compile(book wizparser.Spellbook, chatty bool, emitComments bool) error {
	startTime := time.Now()

	wd, err := os.Getwd()
	if err != nil {
		return errors.Wrap(err, 0)
	}

	fullPath := filepath.Join(wd, "wizardry", "wizbook", "book.go")

	f, err := os.Create(fullPath)
	if err != nil {
		return errors.Wrap(err, 0)
	}

	fmt.Println("Generating into:", fullPath)

	defer f.Close()

	lf := []byte("\n")
	oneIndent := []byte("  ")
	indentLevel := 0

	indent := func() {
		indentLevel++
	}

	outdent := func() {
		indentLevel--
	}

	emit := func(format string, args ...interface{}) {
		if format != "" {
			for i := 0; i < indentLevel; i++ {
				f.Write(oneIndent)
			}
			fmt.Fprintf(f, format, args...)
		}
		f.Write(lf)
	}

	emitLabel := func(label string) {
		// labels have one less indent than usual
		for i := 1; i < indentLevel; i++ {
			f.Write(oneIndent)
		}
		f.Write([]byte(label))
		f.WriteString(":")
		f.Write(lf)
	}

	withIndent := func(f indentCallback) {
		indent()
		f()
		outdent()
	}

	emit("// this file has been generated by github.com/fasterthanlime/wizardry")
	emit("// from a set of magic rules. you probably don't want to edit it by hand")
	emit("")

	emit("package wizbook")
	emit("")
	emit("import (")
	withIndent(func() {
		emit(strconv.Quote("fmt"))
		emit(strconv.Quote("encoding/binary"))
		emit(strconv.Quote("github.com/fasterthanlime/wizardry/wizardry"))
	})
	emit(")")
	emit("")

	emit("// silence import errors, if we don't use string/search etc.")
	emit("var _ wizardry.StringTestFlags")
	emit("var _ fmt.State")

	emit("var l binary.ByteOrder = binary.LittleEndian")
	emit("var b binary.ByteOrder = binary.BigEndian")
	for _, byteWidth := range []byte{1, 2, 4, 8} {
		emit("type i%d int%d", byteWidth, byteWidth*8)
		emit("type u%d uint%d", byteWidth, byteWidth*8)
	}
	emit("gt := wizardry.StringTest")
	emit("ht := wizardry.SearchTest")
	emit("")

	for _, byteWidth := range []byte{1, 2, 4, 8} {
		for _, endianness := range []wizparser.Endianness{wizparser.LittleEndian, wizparser.BigEndian} {
			retType := "u8"

			emit("// reads an unsigned %d-bit %s integer", byteWidth*8, endianness)
			emit("func f%d%s(tb []byte, off i8) (%s, bool) {", byteWidth, endiannessString(endianness, false), retType)
			withIndent(func() {
				emit("if i8(len(tb)) < off+%d {", byteWidth)
				withIndent(func() {
					emit("return 0, false")
				})
				emit("}")

				if byteWidth == 1 {
					emit("return %s(tb[off]), true", retType)
				} else {
					emit("return %s(%s.Uint%d(tb[off:])), true", retType, endiannessString(endianness, false), byteWidth*8)
				}
			})
			emit("}")
			emit("")
		}
	}

	var pages []string
	for page := range book {
		pages = append(pages, page)
	}
	sort.Strings(pages)

	for _, page := range pages {
		nodes := treeify(book[page])

		for _, swapEndian := range []bool{false, true} {
			defaultSeed := 0

			emit("func Identify%s(tb []byte, po i8) ([]string, error) {", pageSymbol(page, swapEndian))
			withIndent(func() {
				emit("var out []string")
				emit("var ss []string; ss = ss[0:]")
				emit("var gf i8; gf &= gf") // globalOffset
				emit("var ra u8; ra &= ra")
				emit("var rb u8; rb &= rb")
				emit("var rc u8; rc &= rc")
				emit("var rA i8; rA &= rA")
				emit("var ka bool; ka = !!ka")
				emit("var kb bool; kb = !!kb")
				emit("var kc bool; kc = !!kc")
				for i := 0; i < 16; i++ {
					name := fmt.Sprintf("d%x", i)
					emit("var %s bool; %s = !!%s", name, name, name)
				}
				emit("")

				emit("m := func (args... string) {")
				withIndent(func() {
					emit("out = append(out, args...)")
				})
				emit("}")

				var emitNode nodeEmitter

				emitNode = func(node *ruleNode, defaultMarker string, prevSiblingNode *ruleNode) {
					rule := node.rule

					canFail := false

					if emitComments {
						emit("// %s", rule.Line)
					}

					// don't bother emitting global offset if no children
					// with relative addresses
					emitGlobalOffset := false
					for _, child := range node.children {
						cof := child.rule.Offset
						if cof.IsRelative || (cof.OffsetType == wizparser.OffsetTypeIndirect && cof.Indirect.IsRelative) {
							emitGlobalOffset = true
							break
						}
					}

					var off Expression

					reuseOffset := false
					if prevSiblingNode != nil {
						pr := prevSiblingNode.rule
						reuseOffset = pr.Offset.Equals(rule.Offset)
					}

					switch rule.Offset.OffsetType {
					case wizparser.OffsetTypeDirect:
						off = &BinaryOp{
							LHS:      &VariableAccess{"po"},
							Operator: OperatorAdd,
							RHS:      &NumberLiteral{rule.Offset.Direct},
						}
						if rule.Offset.IsRelative {
							off = &BinaryOp{
								LHS:      off,
								Operator: OperatorAdd,
								RHS:      &VariableAccess{"gf"},
							}
						}
					case wizparser.OffsetTypeIndirect:
						indirect := rule.Offset.Indirect

						offsetAddress := quoteNumber(indirect.OffsetAddress)
						if indirect.IsRelative {
							offsetAddress = fmt.Sprintf("(gf + %s)", offsetAddress)
						}

						if !reuseOffset {
							emit("ra, ka = f%d%s(tb, %s)",
								indirect.ByteWidth,
								endiannessString(indirect.Endianness, swapEndian),
								offsetAddress)
						}
						canFail = true
						emit("if !ka { goto %s }", failLabel(node))
						var offsetAdjustValue Expression = &NumberLiteral{indirect.OffsetAdjustmentValue}

						if indirect.OffsetAdjustmentIsRelative {
							offsetAdjustAddress := fmt.Sprintf("%s + %s", offsetAddress, quoteNumber(indirect.OffsetAdjustmentValue))
							emit("rb, kb = f%d%s(tb, %s)",
								indirect.ByteWidth,
								endiannessString(indirect.Endianness, swapEndian),
								offsetAdjustAddress)
							emit("if !kb { goto %s }", failLabel(node))
							offsetAdjustValue = &VariableAccess{"i8(rb)"}
						}

						off = &VariableAccess{"i8(ra)"}

						switch indirect.OffsetAdjustmentType {
						case wizparser.AdjustmentAdd:
							off = &BinaryOp{
								LHS:      off,
								Operator: OperatorAdd,
								RHS:      offsetAdjustValue,
							}
						case wizparser.AdjustmentSub:
							off = &BinaryOp{
								LHS:      off,
								Operator: OperatorSub,
								RHS:      offsetAdjustValue,
							}
						case wizparser.AdjustmentMul:
							off = &BinaryOp{
								LHS:      off,
								Operator: OperatorMul,
								RHS:      offsetAdjustValue,
							}
						case wizparser.AdjustmentDiv:
							off = &BinaryOp{
								LHS:      off,
								Operator: OperatorDiv,
								RHS:      offsetAdjustValue,
							}
						}

						if rule.Offset.IsRelative {
							off = &BinaryOp{
								LHS:      off,
								Operator: OperatorAdd,
								RHS:      &VariableAccess{"gf"},
							}
						}
					}

					off = off.Fold()

					switch rule.Kind.Family {
					case wizparser.KindFamilyInteger:
						ik, _ := rule.Kind.Data.(*wizparser.IntegerKind)

						if !ik.MatchAny {
							reuseSibling := false
							if prevSiblingNode != nil {
								pr := prevSiblingNode.rule
								if pr.Offset.Equals(rule.Offset) && pr.Kind.Family == wizparser.KindFamilyInteger {
									pik, _ := pr.Kind.Data.(*wizparser.IntegerKind)
									if pik.ByteWidth == ik.ByteWidth {
										reuseSibling = true
									}
								}
							}

							if !reuseSibling {
								emit("rc, kc = f%d%s(tb, %s)",
									ik.ByteWidth,
									endiannessString(ik.Endianness, swapEndian),
									off,
								)
							}

							lhs := "rc"

							operator := "=="
							switch ik.IntegerTest {
							case wizparser.IntegerTestEqual:
								operator = "=="
							case wizparser.IntegerTestNotEqual:
								operator = "!="
							case wizparser.IntegerTestLessThan:
								operator = "<"
							case wizparser.IntegerTestGreaterThan:
								operator = ">"
							}

							if ik.IntegerTest == wizparser.IntegerTestGreaterThan || ik.IntegerTest == wizparser.IntegerTestLessThan {
								lhs = fmt.Sprintf("i8(i%d(%s))", ik.ByteWidth, lhs)
							}

							if ik.DoAnd {
								lhs = fmt.Sprintf("%s&%s", lhs, quoteNumber(int64(ik.AndValue)))
							}

							switch ik.AdjustmentType {
							case wizparser.AdjustmentAdd:
								lhs = fmt.Sprintf("(%s+%s)", lhs, quoteNumber(ik.AdjustmentValue))
							case wizparser.AdjustmentSub:
								lhs = fmt.Sprintf("(%s-%s)", lhs, quoteNumber(ik.AdjustmentValue))
							case wizparser.AdjustmentMul:
								lhs = fmt.Sprintf("(%s*%s)", lhs, quoteNumber(ik.AdjustmentValue))
							case wizparser.AdjustmentDiv:
								lhs = fmt.Sprintf("(%s/%s)", lhs, quoteNumber(ik.AdjustmentValue))
							}

							rhs := quoteNumber(ik.Value)

							ruleTest := fmt.Sprintf("kc && (%s %s %s)", lhs, operator, rhs)
							canFail = true
							emit("if !(%s) { goto %s }", ruleTest, failLabel(node))
						}
						if emitGlobalOffset {
							gfValue := &BinaryOp{
								LHS:      off,
								Operator: OperatorAdd,
								RHS:      &NumberLiteral{int64(ik.ByteWidth)},
							}
							emit("gf = %s", gfValue.Fold())
						}
					case wizparser.KindFamilyString:
						sk, _ := rule.Kind.Data.(*wizparser.StringKind)
						emit("rA = i8(gt(tb, int(%s), %s, %d))", off, strconv.Quote(string(sk.Value)), sk.Flags)
						canFail = true
						if sk.Negate {
							emit("if rA >= 0 { goto %s }", failLabel(node))
						} else {
							emit("if rA < 0 { goto %s }", failLabel(node))
						}
						if emitGlobalOffset {
							gfValue := &BinaryOp{
								LHS:      off,
								Operator: OperatorAdd,
								RHS:      &VariableAccess{"rA"},
							}
							emit("gf = %s", gfValue.Fold())
						}

					case wizparser.KindFamilySearch:
						sk, _ := rule.Kind.Data.(*wizparser.SearchKind)
						emit("rA = i8(ht(tb, int(%s), %s, %s))", off, quoteNumber(int64(sk.MaxLen)), strconv.Quote(string(sk.Value)))
						canFail = true
						emit("if rA < 0 { goto %s }", failLabel(node))
						if emitGlobalOffset {
							gfValue := &BinaryOp{
								LHS:      off,
								Operator: OperatorAdd,
								RHS: &BinaryOp{
									LHS:      &VariableAccess{"rA"},
									Operator: OperatorAdd,
									RHS:      &NumberLiteral{int64(len(sk.Value))},
								},
							}
							emit("gf = %s", gfValue.Fold())
						}

					case wizparser.KindFamilyUse:
						uk, _ := rule.Kind.Data.(*wizparser.UseKind)
						emit("ss, _ = Identify%s(tb, %s)", pageSymbol(uk.Page, uk.SwapEndian), off)
						emit("m(ss...)")

					case wizparser.KindFamilyName:
						// do nothing, pretty much

					case wizparser.KindFamilyClear:
						// reset defaultMarker for this level
						if defaultMarker == "" {
							panic("compiler error: nil defaultMarker for clear rule")
						}
						emit("%s = false", defaultMarker)

					case wizparser.KindFamilyDefault:
						// only succeed if defaultMarker is unset
						// (so, fail if it's set)
						if defaultMarker == "" {
							panic("compiler error: nil defaultMarker for default rule")
						}
						canFail = true
						emit("if %s { goto %s }", defaultMarker, failLabel(node))
						if emitGlobalOffset {
							emit("gf = %s", off)
						}

					default:
						emit("// fixme: unhandled kind %s", rule.Kind)
						canFail = true
						emit("goto %s", failLabel(node))
					}

					if chatty {
						emit("fmt.Printf(\"%%s\\n\", %s)", strconv.Quote(rule.Line))
					}
					if len(rule.Description) > 0 {
						emit("m(%s)", strconv.Quote(string(rule.Description)))
					}

					numChildren := len(node.children)
					childDefaultMarker := ""

					if numChildren > 0 {
						for _, child := range node.children {
							if child.rule.Kind.Family == wizparser.KindFamilyDefault {
								childDefaultMarker = fmt.Sprintf("d%x", rule.Level)
								defaultSeed++
								emit("%s = false", childDefaultMarker)
								break
							}
						}

						var prevSibling = node
						for _, child := range node.children {
							emitNode(child, childDefaultMarker, prevSibling)
							prevSibling = child
						}
					}

					if defaultMarker != "" {
						emit("%s = true", defaultMarker)
					}

					if canFail {
						emitLabel(failLabel(node))
					}
				}

				for _, node := range nodes {
					emitNode(node, "", nil)
				}

				emit("return out, nil")
			})
			emit("}")
			emit("")
		}

	}

	fmt.Printf("Compiled in %s\n", time.Since(startTime))

	fSize, _ := f.Seek(0, os.SEEK_CUR)
	fmt.Printf("Generated code is %s\n", humanize.IBytes(uint64(fSize)))

	return nil
}

func pageSymbol(page string, swapEndian bool) string {
	result := ""
	for _, token := range strings.Split(page, "-") {
		result += strings.Title(token)
	}

	if swapEndian {
		result += "__Swapped"
	}

	return result
}

func endiannessString(en wizparser.Endianness, swapEndian bool) string {
	if en.MaybeSwapped(swapEndian) == wizparser.BigEndian {
		return "b"
	}
	return "l"
}

func quoteNumber(number int64) string {
	return fmt.Sprintf("%d", number)
}

func treeify(rules []wizparser.Rule) []*ruleNode {
	var rootNodes []*ruleNode
	var nodeStack []*ruleNode
	var idSeed int64

	for _, rule := range rules {
		node := &ruleNode{
			id:   idSeed,
			rule: rule,
		}
		idSeed++

		if rule.Level > 0 {
			parent := nodeStack[rule.Level-1]
			parent.children = append(parent.children, node)
		} else {
			rootNodes = append(rootNodes, node)
		}

		nodeStack = append(nodeStack[0:rule.Level], node)
	}

	return rootNodes
}

func failLabel(node *ruleNode) string {
	return fmt.Sprintf("f%x", node.id)
}
