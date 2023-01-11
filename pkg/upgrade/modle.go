package upgrade

type RootEntity struct {
	Url             string          `json:"url"`
	AssetsUrl       string          `json:"assets_url"`
	UploadUrl       string          `json:"upload_url"`
	HtmlUrl         string          `json:"html_url"`
	Id              int64           `json:"id"`
	NodeId          string          `json:"node_id"`
	TagName         string          `json:"tag_name"`
	TargetCommitish string          `json:"target_commitish"`
	Name            string          `json:"name"`
	Draft           bool            `json:"draft"`
	Prerelease      bool            `json:"prerelease"`
	CreatedAt       string          `json:"created_at"`
	PublishedAt     string          `json:"published_at"`
	Assets          []AssetsEntity  `json:"assets"`
	TarballUrl      string          `json:"tarball_url"`
	ZipballUrl      string          `json:"zipball_url"`
	Body            string          `json:"body"`
	Reactions       ReactionsEntity `json:"reactions"`
}

type AuthorEntity struct {
	Login             string `json:"login"`
	Id                int64  `json:"id"`
	NodeId            string `json:"node_id"`
	AvatarUrl         string `json:"avatar_url"`
	GravatarId        string `json:"gravatar_id"`
	Url               string `json:"url"`
	HtmlUrl           string `json:"html_url"`
	FollowersUrl      string `json:"followers_url"`
	FollowingUrl      string `json:"following_url"`
	GistsUrl          string `json:"gists_url"`
	StarredUrl        string `json:"starred_url"`
	SubscriptionsUrl  string `json:"subscriptions_url"`
	OrganizationsUrl  string `json:"organizations_url"`
	ReposUrl          string `json:"repos_url"`
	EventsUrl         string `json:"events_url"`
	ReceivedEventsUrl string `json:"received_events_url"`
	Type              string `json:"type"`
	SiteAdmin         bool   `json:"site_admin"`
}

type AssetsEntity struct {
	Url                string         `json:"url"`
	Id                 int64          `json:"id"`
	NodeId             string         `json:"node_id"`
	Name               string         `json:"name"`
	Label              string         `json:"label"`
	Uploader           UploaderEntity `json:"uploader"`
	ContentType        string         `json:"content_type"`
	State              string         `json:"state"`
	Size               int64          `json:"size"`
	DownloadCount      int64          `json:"download_count"`
	CreatedAt          string         `json:"created_at"`
	UpdatedAt          string         `json:"updated_at"`
	BrowserDownloadUrl string         `json:"browser_download_url"`
}

type UploaderEntity struct {
	Login             string `json:"login"`
	Id                int64  `json:"id"`
	NodeId            string `json:"node_id"`
	AvatarUrl         string `json:"avatar_url"`
	GravatarId        string `json:"gravatar_id"`
	Url               string `json:"url"`
	HtmlUrl           string `json:"html_url"`
	FollowersUrl      string `json:"followers_url"`
	FollowingUrl      string `json:"following_url"`
	GistsUrl          string `json:"gists_url"`
	StarredUrl        string `json:"starred_url"`
	SubscriptionsUrl  string `json:"subscriptions_url"`
	OrganizationsUrl  string `json:"organizations_url"`
	ReposUrl          string `json:"repos_url"`
	EventsUrl         string `json:"events_url"`
	ReceivedEventsUrl string `json:"received_events_url"`
	Type              string `json:"type"`
	SiteAdmin         bool   `json:"site_admin"`
}

type ReactionsEntity struct {
	Url        string `json:"url"`
	TotalCount int64  `json:"total_count"`
	Normal1    int64  `json:"+1"`
	Normal11   int64  `json:"-1"`
	Laugh      int64  `json:"laugh"`
	Hooray     int64  `json:"hooray"`
	Confused   int64  `json:"confused"`
	Heart      int64  `json:"heart"`
	Rocket     int64  `json:"rocket"`
	Eyes       int64  `json:"eyes"`
}
