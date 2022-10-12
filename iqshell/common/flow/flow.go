package flow

import (
	"github.com/qiniu/qshell/v2/iqshell/common/alert"
	"github.com/qiniu/qshell/v2/iqshell/common/data"
	"github.com/qiniu/qshell/v2/iqshell/common/log"
	"github.com/qiniu/qshell/v2/iqshell/common/workspace"
	"sync"
	"time"
)

type Info struct {
	Force             bool // 是否强制直接进行 Flow, 不强制需要用户输入验证码验证
	WorkerCount       int  // worker 数量
	StopWhenWorkError bool // 当某个 work 遇到执行错误是否结束 batch 任务
}

func (i *Info) Check() *data.CodeError {
	if i.WorkerCount < 1 {
		i.WorkerCount = 1
	}
	return nil
}

type Flow struct {
	Info           Info           // flow 的参数信息 【可选】
	WorkProvider   WorkProvider   // work 提供者 【必填】
	WorkerProvider WorkerProvider // worker 提供者 【必填】

	DoWorkInfoListMaxCount int           // Worker.DoWork 函数中 works 数组最大长度，最小长度为 1
	Limit                  AutoLimit     // 速度限制，用于限制
	EventListener          EventListener // work 处理事项监听者 【可选】
	Overseer               Overseer      // work 监工，涉及 work 是否已处理相关的逻辑 【可选】
	Skipper                Skipper       // work 是否跳过相关逻辑 【可选】
	Redo                   Redo          // work 是否需要重新做相关逻辑，有些工作虽然已经做过，但下次处理时可能条件发生变化，需要重新处理 【可选】
	workErrorHappened      bool          // 执行中是否出现错误 【内部变量】
}

func (f *Flow) Check() *data.CodeError {
	if err := f.Info.Check(); err != nil {
		return err
	}

	if f.WorkProvider == nil {
		return alert.CannotEmptyError("WorkProvider", "")
	}
	if f.WorkerProvider == nil {
		return alert.CannotEmptyError("WorkerProvider", "")
	}

	if f.DoWorkInfoListMaxCount < 1 {
		f.DoWorkInfoListMaxCount = 1
	}

	return nil
}

func (f *Flow) Start() {
	if e := f.Check(); e != nil {
		log.ErrorF("work flow start error:%v", e)
		return
	}

	if !f.Info.Force && !UserCodeVerification() {
		return
	}

	if f.EventListener.FlowWillStartFunc != nil {
		if err := f.EventListener.FlowWillStartFunc(f); err != nil {
			log.ErrorF("Flow start error:%v", err)
			return
		}
	}

	log.Debug("work flow did start")
	workChan := make(chan []*WorkInfo, f.Info.WorkerCount)
	// 生产者
	go func() {
		log.DebugF("work producer start")

		workList := make([]*WorkInfo, 0, f.DoWorkInfoListMaxCount)
		for {
			hasMore, workInfo, err := f.WorkProvider.Provide()
			if err != nil {
				if err.Code == data.ErrorCodeParamMissing {
					f.EventListener.OnWorkSkip(workInfo, nil, err)
				} else {
					f.EventListener.OnWorkFail(workInfo, err)
				}
				continue
			}

			if workInfo == nil || workInfo.Work == nil {
				if !hasMore {
					break
				} else {
					continue
				}
			}

			// 检测 work 是否需要过
			if f.Skipper != nil {
				if skip, cause := f.Skipper.ShouldSkip(workInfo); skip {
					f.EventListener.OnWorkSkip(workInfo, nil, cause)
					continue
				}
			}

			// 检测 work 是否已经做过
			if f.Overseer != nil {
				if hasDone, workRecord := f.Overseer.GetWorkRecordIfHasDone(workInfo); hasDone {
					if f.Redo == nil {
						f.EventListener.OnWorkSkip(workInfo, workRecord.Result, data.NewError(data.ErrorCodeAlreadyDone, workRecord.Err.Error()))
						continue
					}

					if shouldRedo, cause := f.Redo.ShouldRedo(workInfo, workRecord); !shouldRedo {
						if cause == nil {
							cause = data.NewError(data.ErrorCodeAlreadyDone, "already done")
						}
						cause.Code = data.ErrorCodeAlreadyDone
						f.EventListener.OnWorkSkip(workInfo, workRecord.Result, cause)
						continue
					} else {
						if cause == nil {
							log.DebugF("work redo, %s", workInfo.Data)
						} else {
							log.DebugF("work redo, %s because:%v", workInfo.Data, cause.Desc)
						}
					}
				}
			}

			// 通知 work 将要执行
			if shouldContinue, e := f.EventListener.WillWork(workInfo); !shouldContinue {
				f.EventListener.OnWorkSkip(workInfo, nil, e)
				continue
			}

			workList = append(workList, workInfo)
			if len(workList) >= f.DoWorkInfoListMaxCount {
				workChan <- workList
				workList = make([]*WorkInfo, 0, f.DoWorkInfoListMaxCount)
			}
		}

		if len(workList) > 0 {
			workChan <- workList
		}

		close(workChan)
		log.DebugF("work producer   end")
	}()

	// 消费者
	wait := &sync.WaitGroup{}
	wait.Add(f.Info.WorkerCount)
	for i := 0; i < f.Info.WorkerCount; i++ {
		go func(index int) {
			log.DebugF("work consumer %d start", index)
			worker, err := f.WorkerProvider.Provide()
			if err != nil {
				log.ErrorF("Create Worker Error:%v", err)
				return
			}

			for workList := range workChan {
				if workspace.IsCmdInterrupt() {
					break
				}

				if f.Limit != nil {
					_ = f.Limit.Acquire(int64(len(workList)))
				}

				// workRecordList 有数据则长度和 workList 长度相同
				workRecordList, workErr := worker.DoWork(workList)
				if len(workRecordList) == 0 && workErr != nil {
					log.ErrorF("Do Worker Error:%v", err)
					break
				}

				resultHandler := func(workRecord *WorkRecord) {
					if f.Overseer != nil {
						f.Overseer.WorkDone(&WorkRecord{
							WorkInfo: workRecord.WorkInfo,
							Result:   workRecord.Result,
							Err:      workRecord.Err,
						})
					}
					if workRecord.Err != nil {
						f.EventListener.OnWorkFail(workRecord.WorkInfo, workRecord.Err)
						f.workErrorHappened = true
					} else {
						f.EventListener.OnWorkSuccess(workRecord.WorkInfo, workRecord.Result)
					}

					// 是否出发了上限，触发了，减小 limit count

				}

				isHitLimit := func(workRecord *WorkRecord) bool {
					if f.Limit == nil {
						return false
					}

					return f.Limit.IsLimitError(0, workRecord.Err)
				}

				var hitLimitCount int64 = 0
				for _, record := range workRecordList {
					if (record.Result == nil || !record.Result.IsValid()) && record.Err == nil {
						record.Err = workErr
					}
					resultHandler(record)
					if isHitLimit(record) {
						hitLimitCount += 1
					}
				}

				if f.Limit != nil {
					f.Limit.Release(int64(len(workList)))

					if hitLimitCount > 0 {
						f.Limit.AddLimitCount(-1 * hitLimitCount)
						time.Sleep(time.Millisecond * 1500)
					}
				}

				// 检测是否需要停止
				if f.workErrorHappened && f.Info.StopWhenWorkError {
					break
				}
			}

			wait.Done()
			log.DebugF("work consumer %d   end", index)
		}(i)
	}
	wait.Wait()

	if f.EventListener.FlowWillEndFunc != nil {
		if err := f.EventListener.FlowWillEndFunc(f); err != nil {
			log.ErrorF("Flow end error:%v", err)
			return
		}
	}

	log.Debug("work flow did end")
}
