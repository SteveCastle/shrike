package tasks

import (
	"bufio"
	"fmt"
	"strings"
	"sync"

	"github.com/stevecastle/shrike/appconfig"
	"github.com/stevecastle/shrike/embedexec"
	"github.com/stevecastle/shrike/jobqueue"
)

func autotagTask(j *jobqueue.Job, q *jobqueue.Queue, mu *sync.Mutex) error {
	ctx := j.Ctx

	var paths []string
	if qstr, ok := extractQueryFromJob(j); ok {
		q.PushJobStdout(j.ID, fmt.Sprintf("autotag: using query to select files: %s", qstr))
		mediaPaths, err := getMediaPathsByQuery(q.Db, qstr)
		if err != nil {
			q.PushJobStdout(j.ID, "autotag: failed to load paths from query: "+err.Error())
			q.ErrorJob(j.ID)
			return err
		}
		paths = mediaPaths
	} else {
		raw := strings.TrimSpace(j.Input)
		if raw == "" {
			q.PushJobStdout(j.ID, "autotag: no image path provided in job input or query flag")
			q.CompleteJob(j.ID)
			return nil
		}
		paths = parseInputPaths(raw)
	}

	if err := EnsureCategoryExists(q.Db, "Suggested", 0); err != nil {
		q.PushJobStdout(j.ID, "autotag: failed to ensure category: "+err.Error())
		q.ErrorJob(j.ID)
		return err
	}

	if len(paths) == 0 {
		q.PushJobStdout(j.ID, "autotag: no valid paths parsed from input")
		q.CompleteJob(j.ID)
		return nil
	}

	for idx, imagePath := range paths {
		select {
		case <-ctx.Done():
			q.PushJobStdout(j.ID, "autotag: task canceled")
			_ = q.CancelJob(j.ID)
			return ctx.Err()
		default:
		}

		cfg := appconfig.Get()
		args := []string{}
		if strings.TrimSpace(cfg.OnnxTagger.LabelsPath) != "" {
			args = append(args, `--labels=`+cfg.OnnxTagger.LabelsPath)
		}
		if strings.TrimSpace(cfg.OnnxTagger.ConfigPath) != "" {
			args = append(args, `--config=`+cfg.OnnxTagger.ConfigPath)
		}
		if strings.TrimSpace(cfg.OnnxTagger.ModelPath) != "" {
			args = append(args, `--model=`+cfg.OnnxTagger.ModelPath)
		}
		if strings.TrimSpace(cfg.OnnxTagger.ORTSharedLibraryPath) != "" {
			args = append(args, `--ort=`+cfg.OnnxTagger.ORTSharedLibraryPath)
		}
		if cfg.OnnxTagger.GeneralThreshold > 0 {
			args = append(args, `--general-thresh=`+fmt.Sprintf("%g", cfg.OnnxTagger.GeneralThreshold))
		}
		if cfg.OnnxTagger.CharacterThreshold > 0 {
			args = append(args, `--character-thresh=`+fmt.Sprintf("%g", cfg.OnnxTagger.CharacterThreshold))
		}
		args = append(args, `--image=`+imagePath)

		q.PushJobStdout(j.ID, fmt.Sprintf("autotag: [%d/%d] tagging %s", idx+1, len(paths), imagePath))

		cmd, cleanup, err := embedexec.GetExec(ctx, "onnxtag", args...)
		if err != nil {
			q.PushJobStdout(j.ID, "autotag: failed to prepare onnxtag: "+err.Error())
			q.ErrorJob(j.ID)
			return err
		}
		if cleanup != nil {
			defer cleanup()
		}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			q.PushJobStdout(j.ID, "autotag: failed to get stdout pipe: "+err.Error())
			q.ErrorJob(j.ID)
			return err
		}
		stderr, err := cmd.StderrPipe()
		if err != nil {
			q.PushJobStdout(j.ID, "autotag: failed to get stderr pipe: "+err.Error())
			q.ErrorJob(j.ID)
			return err
		}

		doneErr := make(chan struct{})
		go func() {
			s := bufio.NewScanner(stderr)
			for s.Scan() {
				_ = q.PushJobStdout(j.ID, "autotag stderr: "+s.Text())
			}
			close(doneErr)
		}()

		if err := cmd.Start(); err != nil {
			q.PushJobStdout(j.ID, "autotag: failed to start onnxtag: "+err.Error())
			q.ErrorJob(j.ID)
			return err
		}

		var tags []string
		scan := bufio.NewScanner(stdout)
		for scan.Scan() {
			line := strings.TrimSpace(scan.Text())
			if line != "" {
				tags = append(tags, line)
				_ = q.PushJobStdout(j.ID, "autotag: "+line)
			}
		}
		_ = cmd.Wait()
		<-doneErr

		if len(tags) == 0 {
			q.PushJobStdout(j.ID, "autotag: no tags returned")
			continue
		}

		var tagInfos []TagInfo
		for _, t := range tags {
			name := t
			if pos := strings.LastIndex(t, ":"); pos > 0 {
				name = strings.TrimSpace(t[:pos])
			}
			if name == "" {
				continue
			}
			tagInfos = append(tagInfos, TagInfo{Label: name, Category: "Suggested"})
		}

		if err := insertTagsForFile(q.Db, imagePath, tagInfos); err != nil {
			q.PushJobStdout(j.ID, "autotag: failed to insert tags: "+err.Error())
			q.ErrorJob(j.ID)
			return err
		}
		q.PushJobStdout(j.ID, fmt.Sprintf("autotag: wrote %d Suggested tags for %s", len(tagInfos), imagePath))
	}

	q.CompleteJob(j.ID)
	return nil
}
