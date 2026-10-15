import {randomUUID} from 'node:crypto';
import {reserve, finish, activity, controllerActor, terminal, workerEvent, promptsReady} from './domain.js';
import {BuildCatalog} from './builds.js';

export async function reconcile(store, kube, config) {
  if (!await kube.leader()) return;
  config.syncMailbox?.();
  await config.syncGithub?.();
  // Startup and periodic scans cover all durable tasks, never just new deliveries.
  for (const initial of await store.list()) {
    // Finish cleanup even if a previous controller crashed after recording result.
    if (initial.lastExecution?.podName && initial.lastExecution.id !== initial.active?.id) {
      const old = await kube.get('pod', initial.lastExecution.podName);
      if (old && ['Failed', 'Succeeded'].includes(old.status?.phase)) {
        if (old.metadata.finalizers?.includes('agents.codemowers.io/result-recorded')) {
          const finalizers = old.metadata.finalizers.filter(f => f !== 'agents.codemowers.io/result-recorded');
          await kube.run(['patch', 'pod', old.metadata.name, '--type=merge', '-p', JSON.stringify({metadata: {finalizers}})]);
        }
        if (initial.deleteRequested || initial.lastExecution.state !== 'Failed') {
          await kube.run(['delete', 'pod', old.metadata.name, '--ignore-not-found', '--wait=false']);
        }
      }
      await kube.run(['delete', 'configmap', initial.lastExecution.podName + '-input', '--ignore-not-found']).catch(error => {
        if (!/Forbidden/.test(error.message)) throw error;
        console.error(JSON.stringify({level: 'warn', component: 'reconcile', message: 'Cannot delete stale input ConfigMap (RBAC missing delete on configmaps): ' + error.message.slice(0, 300)}));
      });
    }
    if (initial.deleteRequested && !initial.active) {
      // Failed executions can outlive lastExecution when a Task is retried.
      await kube.run(['delete', 'pod,configmap', '-l', `agents.codemowers.io/task-id=${initial.id}`,
        '--ignore-not-found', '--wait=false']);
      const pvcName = `${config.release}-task-${initial.id}`;
      await kube.run(['delete', 'pvc', pvcName, '--ignore-not-found', '--wait=false']);
      if (!await kube.get('pvc', pvcName)) { await config.cleanupTask?.(initial); await store.remove(initial.id); }
      continue;
    }
    if (terminal.has(initial.phase) && !initial.active) continue;
    let task = initial;
    if (!task.active && promptsReady(task) && task.desiredState === 'Active') {
      task = await store.mutate(task.id, randomUUID(), reserve);
    }
    if (!task.active) continue;
    const execution = task.active;
    const pod = await kube.get('pod', execution.podName);
    if (pod && (pod.metadata.labels?.['agents.codemowers.io/execution-id'] !== execution.id
      || pod.metadata.labels?.['agents.codemowers.io/task-id'] !== task.id)) {
      throw new Error('Worker identity mismatch; refusing adoption');
    }
    if (pod && execution.podUID && pod.metadata.uid !== execution.podUID) {
      await store.mutate(task.id, 'identity-mismatch:' + execution.id, current => {
        current.desiredState = 'Paused'; current.phase = 'Paused'; current.reason = 'WorkerIdentityRequiresReview';
        activity(current, controllerActor, 'Worker name was reused with a different Pod UID; recovery requires review.',
          'controller', {kind: 'recovery'});
      });
      continue;
    }
    if (pod && (execution.podPhase !== pod.status?.phase
      || execution.podReady !== Boolean(pod.status?.conditions?.some(c => c.type === 'Ready' && c.status === 'True')))) {
      task = await store.mutate(task.id, randomUUID(), current => {
        if (current.active?.id !== execution.id) return;
        current.active.podPhase = pod.status?.phase || 'Pending';
        current.active.podReady = Boolean(pod.status?.conditions?.some(c => c.type === 'Ready' && c.status === 'True'));
      });
    }
    const exited = pod?.status?.containerStatuses?.find(c => c.name === 'agent')?.state?.terminated;
    const started = pod?.status?.containerStatuses?.find(c => c.name === 'agent')?.state?.running;
    // Stop intent must not wait for log retrieval or parsing to succeed.
    if (pod && !exited && !['Failed','Succeeded'].includes(pod.status?.phase)
      && !pod.metadata.deletionTimestamp && task.desiredState !== 'Active') {
      await store.mutate(task.id,'stopping-worker:'+execution.id,current=>{
        if (current.active?.id===execution.id) activity(current,controllerActor,'Stopping worker','controller',{kind:'execution_stop',executionId:execution.id,podName:execution.podName});
      });
      await kube.run(['delete','pod',execution.podName,'--wait=false']);
      continue;
    }
    if (exited || started) {
      const output = await kube.run(['logs', execution.podName, '-c', 'agent', '--tail=2000','--limit-bytes=2000000']);
      let afterSequence = task.active?.logSequence || 0;
      for (const line of output.split('\n')) {
        if (!line.startsWith('{')) continue;
        let entry; try { entry = JSON.parse(line); } catch { continue; }
        if (entry.type !== 'agent_event' || entry.executionId !== execution.id
          || !Number.isSafeInteger(entry.sequence) || entry.sequence <= afterSequence) continue;
        await store.mutate(task.id, 'log:' + execution.id + ':' + entry.sequence, current => {
          workerEvent(current, execution.id, entry);
        });
        afterSequence = entry.sequence;
      }
      task = await store.get(task.id);
    }
    if (exited || (pod && ['Failed', 'Succeeded'].includes(pod.status?.phase))) {
      // The worker prints its persisted final result as a fallback if reporting failed.
      let result = task.active?.result;
      if (exited) {
        const output = await kube.run(['logs', execution.podName, '-c', 'agent', '--tail=20']);
        for (const line of output.split('\n')) {
          if (!line.startsWith('{')) continue;
          let entry; try { entry = JSON.parse(line); } catch { continue; }
          if (entry.type === 'agent_result' && entry.executionId === execution.id) result = entry.result;
        }
      }
      if (result) result.exitCode = exited?.exitCode ?? (pod.status.phase === 'Failed' ? 1 : result.exitCode);
      const finalResult = result || {exitCode: exited?.exitCode ?? (pod.status.phase === 'Succeeded' ? 0 : 1),
        summary: 'Worker stopped without a final result; inspect execution logs.'};
      await store.mutate(task.id, 'finish:' + execution.id, current => {
        if (current.active?.id === execution.id) finish(current, execution.id, finalResult);
      });
      if (pod.metadata.finalizers?.includes('agents.codemowers.io/result-recorded')) {
        const finalizers = pod.metadata.finalizers.filter(f => f !== 'agents.codemowers.io/result-recorded');
        await kube.run(['patch', 'pod', execution.podName, '--type=merge', '-p', JSON.stringify({metadata: {finalizers}})]);
      }
      if (finalResult.exitCode === 0 && pod.status.phase !== 'Failed') {
        await kube.run(['delete', 'pod', execution.podName, '--ignore-not-found', '--wait=false']);
      }
      await kube.run(['delete', 'configmap', execution.podName + '-input', '--ignore-not-found']).catch(error => {
        if (!/Forbidden/.test(error.message)) throw error;
        console.error(JSON.stringify({level: 'warn', component: 'reconcile', message: 'Cannot delete input ConfigMap (RBAC missing delete on configmaps): ' + error.message.slice(0, 300)}));
      });
      continue;
    }
    if (pod?.metadata.deletionTimestamp) continue;
    if (!pod && execution.podUID) {
      await store.mutate(task.id, 'missing:' + execution.id, current => {
        current.reason = 'WorkerMissingRequiresReview'; current.desiredState = 'Paused';
        activity(current, controllerActor, 'Recorded worker disappeared; paused for safe recovery.', 'controller', {kind: 'recovery'});
      });
      continue;
    }
    if (task.desiredState !== 'Active') {
      if (pod) await kube.run(['delete', 'pod', execution.podName, '--wait=false']);
      else await store.mutate(task.id, 'stopped:' + execution.id, current => {
        if (current.active?.id === execution.id) finish(current, execution.id, {exitCode: 143, summary: 'Execution stopped by requested state.'});
      });
      continue;
    }
    if (pod) {
      // Adopt live Pods after startup. Never replace unreachable/terminating Pods.
      if (!execution.podUID) await store.mutate(task.id, 'adopt:' + execution.id, current => {
        if (current.active?.id === execution.id) {
          current.active.podUID = pod.metadata.uid;
          if (current.active.state === 'Preparing') current.active.state = pod.status?.phase || 'Pending';
        }
      });
      continue;
    }
    try {
      await kube.ensure(kube.pvc(task));
      await kube.ensure(kube.envelope(task));
      await kube.ensure(await kube.workerPod(task, await new BuildCatalog(kube).list()));
    } catch (error) {
      // Quota/admission/scheduling failures keep the reserved execution intact.
      if (!/exceeded quota|Forbidden|admission|failed calling webhook|Conflict/.test(error.message)) throw error;
      await store.mutate(task.id, randomUUID(), current => {
        current.phase = 'Pending'; current.reason = 'AdmissionBlocked';
        current.error = error.message.slice(0, 2000);
      });
    }
  }
}
