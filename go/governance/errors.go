package governance

import "fmt"

// PolicyError 是业务拒绝错误，带机器可读的错误码（如 EXCEED_LIMIT）。
//
// 【Go 差异】Python 用 raise PolicyDenied(code, msg) 抛异常，沿调用栈自动向上传播；
// Go 是显式返回 error，再用 errors.As 把具体类型取出来读 Code。
// 好处是错误路径全部写在代码里看得见，代价是每一层都要手动传。
type PolicyError struct {
	Code    string
	Message string
}

func (e *PolicyError) Error() string { return e.Message }

// Denied 构造一个业务拒绝错误。
func Denied(code, message string) error {
	return &PolicyError{Code: code, Message: message}
}

// TransientError 表示瞬时故障（网络抖动一类）。
// 只有只读或幂等的工具，框架才会自动重试它。
type TransientError struct {
	Err error
}

func (e *TransientError) Error() string { return fmt.Sprintf("transient: %v", e.Err) }
func (e *TransientError) Unwrap() error { return e.Err }

// Transient 把一个普通错误标记成可重试。
func Transient(err error) error {
	return &TransientError{Err: err}
}

// FieldError 是单个字段的校验失败信息，对应 pydantic 报错里的一项。
type FieldError struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

// ValidationError 收集一次解码/校验中发现的所有字段问题。
//
// 之所以做成"收集全部"而不是"遇到第一个就返回"，是因为这份错误要交给模型看：
// 一次把该改的字段都告诉它，比让它试一次改一个要省好几轮对话。
type ValidationError struct {
	Fields []FieldError
}

func (e *ValidationError) Error() string {
	if len(e.Fields) == 0 {
		return "invalid argument"
	}
	return fmt.Sprintf("%s: %s", e.Fields[0].Path, e.Fields[0].Message)
}

// Add 追加一条字段错误。
func (e *ValidationError) Add(path, message string) {
	e.Fields = append(e.Fields, FieldError{Path: path, Message: message})
}

// OrNil 在没有任何字段出错时返回 nil。
//
// 【Go 坑】这里必须返回 error 接口的 nil，不能直接 return e——
// 一个值为 nil 的 *ValidationError 装进 error 接口后，err != nil 仍然成立。
func (e *ValidationError) OrNil() error {
	if len(e.Fields) == 0 {
		return nil
	}
	return e
}
