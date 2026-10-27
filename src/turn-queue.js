// basic.get is not governed by consumer prefetch. Bound collection to the queue
// snapshot so continuous producers cannot prevent the next model turn starting.
export async function queuedDeliveries(channel, queue) {
  const first = await channel.get(queue, {noAck: false});
  if (!first) return [];
  const deliveries = [first];
  const waiting = first.fields.messageCount;
  for (let remaining = waiting; remaining > 0; remaining--) {
    const delivery = await channel.get(queue, {noAck: false});
    if (!delivery) break;
    deliveries.push(delivery);
  }
  return deliveries;
}

export function turnMessage(messages) {
  if (messages.length === 1) return messages[0];
  return {...messages[0], messageIds: messages.map(message => message.messageId).filter(id => typeof id === 'string' && id.length <= 160).slice(-1000), body:
    'The following inputs were pending before this turn, including any interrupted request. '
    + 'Read all inputs before acting. Later instructions take precedence when they conflict '
    + 'with earlier instructions. Continue the task according to the latest steering.\n\n'
    + messages.map((message, index) => `--- Input ${index + 1} (${message.type}) ---\n${message.body}`).join('\n\n')};
}
