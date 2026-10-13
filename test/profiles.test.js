import test from 'node:test';
import assert from 'node:assert/strict';
import {githubUsername} from '../src/profiles.js';
import {sharedTask} from '../src/sharing.js';

test('GitHub profile names are bounded and shared views retain them without exposing OIDC subjects', () => {
  assert.equal(githubUsername({type: 'user', subject: 'alexriver'}), 'alexriver');
  assert.equal(githubUsername({type: 'agent'}, {username: 'agent-smith'}), 'agent-smith');
  assert.equal(githubUsername({type: 'external', provider: 'github', name: 'octocat'}), 'octocat');
  for (const subject of ['12345', 'https://evil.test', 'bad/name', '11111111-1111-4111-8111-111111111111', 'Human Name']) {
    assert.equal(githubUsername({type: 'user', subject}), null);
  }
  const view = sharedTask({creator: {type: 'user', subject: 'private-id', githubUsername: 'octocat', issuer: 'private'},
    events: [], references: []});
  assert.equal(view.creator.githubUsername, 'octocat');
  assert.equal(view.creator.subject, undefined); assert.equal(view.creator.issuer, undefined);
});
