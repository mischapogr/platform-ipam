from django.urls import path

from .views import WorkspaceView


urlpatterns = [
    path("", WorkspaceView.as_view(), name="workspace"),
]
