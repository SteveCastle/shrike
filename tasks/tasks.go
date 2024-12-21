package tasks

type Task struct {
	ID   string       `json:"id"`
	Name string       `json:"name"`
	Fn   func() error `json:"-"`
}

type TaskMap map[string]Task

var tasks TaskMap

func init() {
	tasks = make(TaskMap)
}

func RegisterTask(id, name string, fn func() error) {
	tasks[id] = Task{
		ID:   id,
		Name: name,
		Fn:   fn,
	}
}

func GetTask(id string) Task {
	return tasks[id]
}

func GetTasks() TaskMap {

	return tasks
}