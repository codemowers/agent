package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// Feed is a public RSS, Atom or JSON Feed that the feed crawler polls.
// Its status holds the crawl state and its most recent items, which workers
// read through the feeds MCP.
type Feed struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              FeedSpec   `json:"spec"`
	Status            FeedStatus `json:"status,omitempty"`
}
type FeedList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Feed `json:"items"`
}
type FeedSpec struct {
	URL string `json:"url"`
	// Interval between polls; nil defaults to one hour.
	Interval *metav1.Duration `json:"interval,omitempty"`
}
type FeedStatus struct {
	ETag         string `json:"etag,omitempty"`
	LastModified string `json:"lastModified,omitempty"`
	// Items are the most recently seen items, newest first by FirstSeen.
	// Fetched items absent from them are new.
	Items         []FeedItem         `json:"items,omitempty"`
	LastPolledAt  *metav1.Time       `json:"lastPolledAt,omitempty"`
	LastChangedAt *metav1.Time       `json:"lastChangedAt,omitempty"`
	Conditions    []metav1.Condition `json:"conditions,omitempty"`
}

// FeedItem is a feed entry as the crawler first saw it.
type FeedItem struct {
	ID        string       `json:"id"`
	Title     string       `json:"title,omitempty"`
	Link      string       `json:"link,omitempty"`
	Published *metav1.Time `json:"published,omitempty"`
	// FirstSeen is when the crawler first fetched the item; unlike
	// Published it only moves forward, so it serves as a cursor.
	FirstSeen metav1.Time `json:"firstSeen"`
}

// FeedBinding wakes a Task of its namespace when its Feed has new items.
type FeedBinding struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              FeedBindingSpec `json:"spec"`
}
type FeedBindingList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []FeedBinding `json:"items"`
}
type FeedBindingSpec struct {
	FeedRef string `json:"feedRef"`
	TaskRef string `json:"taskRef"`
}

func (in *Feed) DeepCopyObject() runtime.Object {
	out := *in
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	if in.Spec.Interval != nil {
		value := *in.Spec.Interval
		out.Spec.Interval = &value
	}
	out.Status.Items = make([]FeedItem, len(in.Status.Items))
	for i, item := range in.Status.Items {
		out.Status.Items[i] = item
		if item.Published != nil {
			value := *item.Published
			out.Status.Items[i].Published = &value
		}
	}
	if in.Status.LastPolledAt != nil {
		value := *in.Status.LastPolledAt
		out.Status.LastPolledAt = &value
	}
	if in.Status.LastChangedAt != nil {
		value := *in.Status.LastChangedAt
		out.Status.LastChangedAt = &value
	}
	out.Status.Conditions = make([]metav1.Condition, len(in.Status.Conditions))
	for i := range in.Status.Conditions {
		in.Status.Conditions[i].DeepCopyInto(&out.Status.Conditions[i])
	}
	return &out
}
func (in *FeedList) DeepCopyObject() runtime.Object {
	out := *in
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	out.Items = make([]Feed, len(in.Items))
	for i := range in.Items {
		out.Items[i] = *(in.Items[i].DeepCopyObject().(*Feed))
	}
	return &out
}
func (in *FeedBinding) DeepCopyObject() runtime.Object {
	out := *in
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	return &out
}
func (in *FeedBindingList) DeepCopyObject() runtime.Object {
	out := *in
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	out.Items = make([]FeedBinding, len(in.Items))
	for i := range in.Items {
		out.Items[i] = *(in.Items[i].DeepCopyObject().(*FeedBinding))
	}
	return &out
}
