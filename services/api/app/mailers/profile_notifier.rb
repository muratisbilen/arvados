# Copyright (C) The Arvados Authors. All rights reserved.
#
# SPDX-License-Identifier: AGPL-3.0

class ProfileNotifier < ActionMailer::Base
  default from: Rails.configuration.Users["AdminNotifierEmailFrom"]

  def profile_created(user, address)
    @user = user
    mail(to: address, subject: "Profile created by #{@user.email}")
  end
end
