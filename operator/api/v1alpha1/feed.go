package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Feed is a public RSS, Atom or JSON Feed that the feed crawler polls.
// Its status holds the crawl state and its most recent items, which workers
// read through the feeds MCP.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="URL",type=string,JSONPath=".spec.url"
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=".status.conditions[?(@.type==\"Ready\")].status"
// +kubebuilder:printcolumn:name="Polled",type=date,JSONPath=".status.lastPolledAt"
// +kubebuilder:printcolumn:name="Changed",type=date,JSONPath=".status.lastChangedAt"
type Feed struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              FeedSpec   `json:"spec"`
	Status            FeedStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type FeedList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Feed `json:"items"`
}
type FeedSpec struct {
	// +kubebuilder:validation:MaxLength=2048
	// +kubebuilder:validation:Pattern=`^https://`
	URL string `json:"url"`
	// Interval between polls; nil defaults to one hour.
	Interval *metav1.Duration `json:"interval,omitempty"`
}
type FeedStatus struct {
	ETag         string `json:"etag,omitempty"`
	LastModified string `json:"lastModified,omitempty"`
	// Items are the most recently seen items, newest first by FirstSeen.
	// Fetched items absent from them are new.
	// +kubebuilder:validation:MaxItems=200
	Items         []FeedItem         `json:"items,omitempty"`
	LastPolledAt  *metav1.Time       `json:"lastPolledAt,omitempty"`
	LastChangedAt *metav1.Time       `json:"lastChangedAt,omitempty"`
	Conditions    []metav1.Condition `json:"conditions,omitempty"`
}

// FeedItem is a feed entry as the crawler first saw it.
type FeedItem struct {
	// +kubebuilder:validation:MaxLength=1024
	ID string `json:"id"`
	// +kubebuilder:validation:MaxLength=512
	Title string `json:"title,omitempty"`
	// +kubebuilder:validation:MaxLength=2048
	Link      string       `json:"link,omitempty"`
	Published *metav1.Time `json:"published,omitempty"`
	// FirstSeen is when the crawler first fetched the item; unlike
	// Published it only moves forward, so it serves as a cursor.
	FirstSeen metav1.Time `json:"firstSeen"`
}

// FeedBinding wakes a Task of its namespace when its Feed has new items.
// +kubebuilder:object:root=true
// +kubebuilder:printcolumn:name="Feed",type=string,JSONPath=".spec.feedRef"
// +kubebuilder:printcolumn:name="Task",type=string,JSONPath=".spec.taskRef"
type FeedBinding struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              FeedBindingSpec `json:"spec"`
}

// +kubebuilder:object:root=true
type FeedBindingList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []FeedBinding `json:"items"`
}
type FeedBindingSpec struct {
	FeedRef string `json:"feedRef"`
	TaskRef string `json:"taskRef"`
}
